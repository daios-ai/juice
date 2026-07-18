package native

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	_ = db.CreateUser(context.Background(), &kernel.User{
		ID: issuerID, Handle: "@_issuer",
		PasswordHash: hash, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = issuerID
	cfg.SigningKey = priv
	return kernel.New(db, nil, nil, &fakeEmbedder{}, cfg, nil), db
}

func seedOwner(t *testing.T, st kernel.Store, handle string) *kernel.User {
	t.Helper()
	hash, _ := kernel.HashPassword("pw")
	u := &kernel.User{
		ID: uuid.New().String(), Handle: handle,
		PasswordHash: hash, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("seedOwner %s: %v", handle, err)
	}
	return u
}

func seedAction(t *testing.T, st kernel.Store, ownerID, name, desc string) *kernel.Action {
	t.Helper()
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: ownerID, Name: name,
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Description: desc,
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

	owner := seedOwner(t, st, "@alice")
	seedAction(t, st, owner.ID, "weather", "weather forecast temperature rain")

	result, err := executeLookup(ctx, map[string]any{"query": "weather forecast"}, "", k)
	if err != nil {
		t.Fatalf("executeLookup: %v", err)
	}
	items, ok := result["results"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("expected non-empty results, got %v", result)
	}
	first, _ := items[0].(map[string]any)
	if first["action"] != "@alice/weather" {
		t.Errorf("expected @alice/weather first, got %v", first["action"])
	}
	for _, required := range []string{"action_id", "score", "input_schema", "output_schema"} {
		if _, ok := first[required]; !ok {
			t.Errorf("result missing %q", required)
		}
	}
	// The consolidated ref replaces the separate name/owner_handle fields.
	for _, banned := range []string{"name", "owner_handle", "price", "uses", "failures"} {
		if _, ok := first[banned]; ok {
			t.Errorf("result must not include field %q", banned)
		}
	}
}

func TestExecuteLookup_DefaultLimitIsTen(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()

	owner := seedOwner(t, st, "@bob")
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

	owner := seedOwner(t, st, "@carol")
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
