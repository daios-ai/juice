package kernel

import (
	"context"
	"testing"
	"time"

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
		sq := sqrt32(norm)
		for i := range vec {
			vec[i] /= sq
		}
	}
	return vec, nil
}

func newTestKernelWithEmbedder(st Store, emb Embedder) *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.FeeBPS = 2000
	cfg.IssuerUserID = "test-issuer-id"
	return New(st, nil, nil, emb, nil, cfg, nil)
}

func TestLookupRanking(t *testing.T) {
	st := newFakeStore()
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	for _, desc := range []struct{ name, text string }{
		{"/weather", "weather forecast temperature rain"},
		{"/news", "latest news headlines today"},
	} {
		a := &Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: desc.name,
			Kind: KindHTTP, Active: true, Description: desc.text,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		vec, _ := emb.Embed(ctx, desc.text)
		_ = st.UpdateActionEmbedding(ctx, a.ID, vec)
	}

	results, err := k.Lookup(ctx, LookupRequest{Query: "weather forecast", Limit: 10})
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
	st := newFakeStore()
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	emb := &fakeEmbedder{}
	owner := setupUser(t, st, "@alice", 0)
	ids := map[string]string{}
	for _, name := range []string{"/reliable", "/unreliable"} {
		a := &Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: name,
			Kind: KindHTTP, Active: true, Description: "compute data results",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		vec, _ := emb.Embed(ctx, "compute data results")
		_ = st.UpdateActionEmbedding(ctx, a.ID, vec)
		ids[name] = a.ID
	}
	_ = st.UpsertStats(ctx, &Stats{ActionID: ids["/reliable"], Uses: 10, Successes: 10, LastUsedAt: time.Now()})
	_ = st.UpsertStats(ctx, &Stats{ActionID: ids["/unreliable"], Uses: 10, Successes: 2, LastUsedAt: time.Now()})

	results, err := k.Lookup(ctx, LookupRequest{Query: "compute data", Limit: 10})
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
	st := newFakeStore()
	k := newTestKernel(st)
	_, err := k.Lookup(context.Background(), LookupRequest{Query: "test"})
	if err == nil {
		t.Error("expected error when no embedder configured")
	}
}

func TestLookupInactiveActionsExcluded(t *testing.T) {
	st := newFakeStore()
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	_ = st.CreateAction(ctx, &Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/hidden",
		Kind: KindHTTP, Active: false, Description: "hidden service do not show",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	results, _ := k.Lookup(ctx, LookupRequest{Query: "hidden service", Limit: 10})
	for _, r := range results {
		if r.Action.Name == "/hidden" {
			t.Error("inactive action should not appear in lookup results")
		}
	}
}
