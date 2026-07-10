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
	return kernel.New(st, nil, nil, emb, cfg, nil)
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

// TestLookupDemotesFailingBelowUntested: with the Laplace-smoothed quality (1+S)/(2+U), an action
// that always fails ranks BELOW an untested one (which ties an unproven action at 0.5) — where the
// old 0.5-floor formula tied them. All three share a description so cosine similarity is equal and
// quality alone orders them: reliable (11/12) > untested (1/2) > failing (1/12).
func TestLookupDemotesFailingBelowUntested(t *testing.T) {
	st := newTestStore(t)
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "@alice", 0)
	ids := map[string]string{}
	for _, name := range []string{"/reliable", "/untested", "/failing"} {
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
	// /untested has NO stats row (quality 0.5).
	_ = st.UpsertStats(ctx, &kernel.Stats{ActionID: ids["/failing"], Uses: 10, Successes: 0, LastUsedAt: time.Now()})

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "compute data", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	order := []string{results[0].Action.Name, results[1].Action.Name, results[2].Action.Name}
	want := []string{"/reliable", "/untested", "/failing"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("ranking = %v, want %v (failing must rank below untested)", order, want)
		}
	}
}

// TestLookupLexicalDegradedMode: with no embedder configured, lookup falls back to the lexical
// (BM25) leg instead of failing — and activation still populates the lexical index without an LLM
// (indexForLookup calls UpsertLookupText regardless of the embedder). So a keyword query finds the
// action on an LLM-less kernel.
func TestLookupLexicalDegradedMode(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st) // no embedder
	ctx := context.Background()
	owner := setupUser(t, st, "@alice", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/weather",
		Kind: kernel.KindHTTP, Active: false, Public: true,
		Description:  "forecast temperature and rain",
		Source:       "https://example.com/api",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "weather forecast", Limit: 10})
	if err != nil {
		t.Fatalf("lookup without an embedder should not error: %v", err)
	}
	if !containsAction(results, a.ID) {
		t.Error("action should be found via the lexical leg with no embedder")
	}
}

// TestLookupSkipsMismatchedEmbedding: a stored vector whose dimension differs from the query's
// (e.g. after an embed-model change) is skipped, not panicked on — and the action stays findable
// via the lexical leg.
func TestLookupSkipsMismatchedEmbedding(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{}) // 8-dim
	ctx := context.Background()
	owner := setupUser(t, st, "@alice", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/x",
		Kind: kernel.KindHTTP, Active: true, Public: true, Description: "unique widget",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_ = st.UpsertEmbedding(ctx, a.ID, []float32{1, 2, 3, 4}) // 4 dims — mismatched
	_ = st.UpsertLookupText(ctx, a.ID, "/x unique widget")

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "widget", Limit: 10})
	if err != nil {
		t.Fatalf("lookup with a mismatched-dimension vector must not error: %v", err)
	}
	if !containsAction(results, a.ID) {
		t.Error("action should still be found lexically despite a bad embedding")
	}
}

// TestLookupFillsPastUncallable: CanCall filtering happens before truncation, so a run of
// uncallable (others' private) matches cannot starve the caller of a callable result.
func TestLookupFillsPastUncallable(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st) // lexical-only, deterministic
	ctx := context.Background()
	alice := setupUser(t, st, "@alice", 0)
	bob := setupUser(t, st, "@bob", 0)

	mk := func(owner *kernel.User, name string, public bool) *kernel.Action {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: name,
			Kind: kernel.KindHTTP, Active: true, Public: public, Description: "widget service",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		_ = st.UpsertLookupText(ctx, a.ID, name+" widget service")
		return a
	}
	_ = mk(bob, "/bobpriv", false)      // matches, NOT callable by alice
	pub := mk(alice, "/alicepub", true) // matches, callable

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "widget service", Limit: 1, CallerID: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action.ID != pub.ID {
		t.Fatalf("expected the callable action to fill the single slot past the uncallable one; got %d results", len(results))
	}
}

// TestLookupQuerySanitized: a query containing FTS5 operators/quotes is treated as literal terms,
// not a MATCH expression — so it never errors.
func TestLookupQuerySanitized(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "@alice", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/db",
		Kind: kernel.KindHTTP, Active: true, Public: true, Description: "query AND filter",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_ = st.CreateAction(ctx, a)
	_ = st.UpsertLookupText(ctx, a.ID, "/db query AND filter")

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: `"AND OR (unbalanced`, Limit: 10})
	if err != nil {
		t.Fatalf("a malformed FTS query must be sanitized, not error: %v", err)
	}
	if !containsAction(results, a.ID) {
		t.Error("expected the literal term AND to match after sanitization")
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
