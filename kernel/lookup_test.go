package kernel_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

type fakeEmbedder struct{}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	dims := 8
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

func newTestKernelWithEmbedder(st kernel.Store, emb kernel.Embedder) *kernel.Kernel {
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	cfg.IssuerUserID = testIssuerUserID
	return kernel.New(st, nil, nil, emb, nil, cfg, nil)
}

func TestLookupRanking(t *testing.T) {
	st := newTestStore(t)
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	for _, desc := range []struct{ name, text string }{
		{"/weather", "weather forecast temperature rain"},
		{"/news", "latest news headlines today"},
	} {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: desc.name,
			Kind: kernel.KindHTTP, Active: true, Public: true, Description: desc.text,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		vec, _ := emb.Embed(ctx, desc.text)
		_ = st.UpsertEmbedding(ctx, a.ID, vec)
	}

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "weather forecast", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Action.Name != "/weather" {
		t.Errorf("expected /weather first, got %s", results[0].Action.Name)
	}
	for _, r := range results {
		if r.Score < 0 {
			t.Errorf("score should be non-negative: %f", r.Score)
		}
		if r.OwnerHandle != "@alice" {
			t.Errorf("expected owner handle @alice, got %q", r.OwnerHandle)
		}
		if r.Action.Description == "" {
			t.Errorf("expected non-empty description for %s", r.Action.Name)
		}
	}
}

func TestLookupRankingWithStats(t *testing.T) {
	st := newTestStore(t)
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	ids := map[string]string{}
	for _, name := range []string{"/reliable", "/unreliable"} {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: name,
			Kind: kernel.KindHTTP, Active: true, Public: true, Description: "compute data results",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		ids[name] = a.ID
		vec, _ := emb.Embed(ctx, a.Description)
		_ = st.UpsertEmbedding(ctx, a.ID, vec)
	}
	_ = st.UpsertStats(ctx, &kernel.Stats{ActionID: ids["/reliable"], Uses: 10, Successes: 10, LastUsedAt: time.Now()})
	_ = st.UpsertStats(ctx, &kernel.Stats{ActionID: ids["/unreliable"], Uses: 10, Successes: 2, LastUsedAt: time.Now()})

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "compute data", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatal("expected at least 2 results")
	}
	if results[0].Action.Name != "/reliable" {
		t.Errorf("reliable action should rank first; got %s", results[0].Action.Name)
	}
}

func TestLookupNoEmbedder(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	_, err := k.Lookup(context.Background(), kernel.LookupRequest{Query: "test"})
	if err == nil {
		t.Error("expected error when no embedder configured")
	}
}

func TestLookupInactiveActionsExcluded(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	_ = st.CreateAction(ctx, &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/hidden",
		Kind: kernel.KindHTTP, Active: false, Description: "hidden service do not show",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	results, _ := k.Lookup(ctx, kernel.LookupRequest{Query: "hidden service", Limit: 10})
	for _, r := range results {
		if r.Action.Name == "/hidden" {
			t.Error("inactive action should not appear in lookup results")
		}
	}
}

func TestLookupACLGrantedNonPublicActionVisible(t *testing.T) {
	st := newTestStore(t)
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	granted := setupUser(t, st, "@bob", 0)
	other := setupUser(t, st, "@carol", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/private-svc",
		Kind: kernel.KindHTTP, Active: true, Public: false,
		Description: "private service only for granted users",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	vec, _ := emb.Embed(ctx, a.Description)
	_ = st.UpsertEmbedding(ctx, a.ID, vec)

	// Grant bob call permission.
	_ = st.GrantACL(ctx, &kernel.ACLEntry{
		SubjectUserID: granted.ID, ActionID: a.ID, Permission: kernel.PermCall,
		CreatedAt: time.Now().UTC(),
	})

	// Owner sees their own non-public action.
	ownerResults, err := k.Lookup(ctx, kernel.LookupRequest{Query: "private service", Limit: 10, SubjectID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !containsAction(ownerResults, a.ID) {
		t.Error("owner should see their own non-public action in lookup")
	}

	// Granted user sees the action.
	grantedResults, err := k.Lookup(ctx, kernel.LookupRequest{Query: "private service", Limit: 10, SubjectID: granted.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !containsAction(grantedResults, a.ID) {
		t.Error("ACL-granted user should see non-public action in lookup")
	}

	// Ungranted user does not see it.
	otherResults, err := k.Lookup(ctx, kernel.LookupRequest{Query: "private service", Limit: 10, SubjectID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if containsAction(otherResults, a.ID) {
		t.Error("ungranted user must not see non-public action in lookup")
	}
}

func containsAction(results []*kernel.LookupResult, actionID string) bool {
	for _, r := range results {
		if r.Action.ID == actionID {
			return true
		}
	}
	return false
}

func TestLookupEmbeddingStoredOnActivate(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	sys := setupUser(t, st, "@sys", 0)
	_ = sys

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/svc",
		Kind: kernel.KindHTTP, Active: false, Public: true,
		Description: "unique service description for lookup",
		Source:      "https://example.com/api",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string", "description": "query"}}},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"r": map[string]any{"type": "string", "description": "result"}}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	// Activate via kernel so storeEmbedding is called.
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "unique service", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range results {
		if r.Action.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Error("activated action should appear in lookup results via stored embedding")
	}
}
