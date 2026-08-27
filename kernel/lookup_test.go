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
	return kernel.New(kernel.Dependencies{Store: st, Embedder: emb, Config: cfg})
}

func TestLookupRanking(t *testing.T) {
	st := newTestStore(t)
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "alice", 0)
	for _, desc := range []struct{ name, text string }{
		{"/weather", "weather forecast temperature rain"},
		{"/news", "latest news headlines today"},
	} {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: desc.name,
			Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Description: desc.text,
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
		if r.OwnerHandle != "alice" {
			t.Errorf("expected owner handle @alice, got %q", r.OwnerHandle)
		}
		if r.Action.Description == "" {
			t.Errorf("expected non-empty description for %s", r.Action.Name)
		}
	}
}

// TestLookupRankingWithFakeEmbeddings: ranking is by fused lexical (BM25) + semantic (cosine)
// relevance, using the fake embedder for the semantic leg. A description that matches the query
// outranks an unrelated one.
func TestLookupRankingWithFakeEmbeddings(t *testing.T) {
	st := newTestStore(t)
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "alice", 0)
	descs := map[string]string{"/match": "compute data results", "/other": "unrelated banana topic"}
	for name, desc := range descs {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: name,
			Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Description: desc,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		vec, _ := emb.Embed(ctx, desc)
		_ = st.UpsertEmbedding(ctx, a.ID, vec)
	}

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "compute data", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 1 {
		t.Fatal("expected at least 1 result")
	}
	if results[0].Action.Name != "/match" {
		t.Errorf("more relevant action should rank first; got %s", results[0].Action.Name)
	}
}

// TestLookupIgnoresStatsWhileQualityUnderRevision: the stats-based quality multiplier is temporarily
// removed (UNDER REVISION — see ranking.md / kernel.go Lookup). Each action's score now depends only
// on relevance, so flipping which action holds the good vs bad success record leaves every score
// unchanged. Before the removal each score moved with its own (1+S)/(2+U). Distinct descriptions give
// the two actions stable, distinct relevance ranks so any score change is attributable to stats.
func TestLookupIgnoresStatsWhileQualityUnderRevision(t *testing.T) {
	st := newTestStore(t)
	emb := &fakeEmbedder{}
	k := newTestKernelWithEmbedder(st, emb)
	ctx := context.Background()

	owner := setupUser(t, st, "alice", 0)
	ids := map[string]string{}
	descs := map[string]string{"/a": "compute data results", "/b": "compute data metrics"}
	for name, desc := range descs {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: name,
			Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Description: desc,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		ids[name] = a.ID
		vec, _ := emb.Embed(ctx, desc)
		_ = st.UpsertEmbedding(ctx, a.ID, vec)
	}

	scores := func() map[string]float32 {
		results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "compute data", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]float32{}
		for _, r := range results {
			m[r.Action.Name] = r.Score
		}
		return m
	}

	_ = st.UpsertStats(ctx, &kernel.Stats{ActionID: ids["/a"], Uses: 10, Successes: 10, LastUsedAt: time.Now()})
	_ = st.UpsertStats(ctx, &kernel.Stats{ActionID: ids["/b"], Uses: 10, Successes: 0, LastUsedAt: time.Now()})
	before := scores()

	// Swap the success records; relevance is unchanged, so scores must not move.
	_ = st.UpsertStats(ctx, &kernel.Stats{ActionID: ids["/a"], Uses: 10, Successes: 0, LastUsedAt: time.Now()})
	_ = st.UpsertStats(ctx, &kernel.Stats{ActionID: ids["/b"], Uses: 10, Successes: 10, LastUsedAt: time.Now()})
	after := scores()

	for _, name := range []string{"/a", "/b"} {
		if before[name] != after[name] {
			t.Fatalf("score of %s changed with stats (%v→%v); quality must be inert while under revision",
				name, before[name], after[name])
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
	owner := setupUser(t, st, "alice", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/weather",
		Kind: kernel.KindHTTP, Active: false, Visibility: kernel.VisibilityPublic,
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
	owner := setupUser(t, st, "alice", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/x",
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Description: "unique widget",
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
	alice := setupUser(t, st, "alice", 0)
	bob := setupUser(t, st, "bob", 0)

	mk := func(owner *kernel.Account, name string, vis kernel.ActionVisibility) *kernel.Action {
		a := &kernel.Action{
			ID: uuid.New().String(), OwnerUserID: owner.ID, Name: name,
			Kind: kernel.KindHTTP, Active: true, Visibility: vis, Description: "widget service",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		_ = st.CreateAction(ctx, a)
		_ = st.UpsertLookupText(ctx, a.ID, name+" widget service")
		return a
	}
	_ = mk(bob, "/bobpriv", kernel.VisibilityPrivate)      // matches, NOT callable by alice
	pub := mk(alice, "/alicepub", kernel.VisibilityPublic) // matches, callable

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
	owner := setupUser(t, st, "alice", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/db",
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic, Description: "query AND filter",
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

	owner := setupUser(t, st, "alice", 0)
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

	owner := setupUser(t, st, "alice", 0)
	sys := setupUser(t, st, "sys", 0)
	_ = sys

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/svc",
		Kind: kernel.KindHTTP, Active: false, Visibility: kernel.VisibilityPublic,
		Description:  "unique service description for lookup",
		Source:       "https://example.com/api",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string", "description": "query"}}},
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"r": map[string]any{"type": "string", "description": "result"}}},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
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

// TestLookupMatchesOwnerHandle: an action's real name is @owner/name, so a query naming the owner
// must find it via the lexical leg even though the handle appears nowhere in its description. No
// embedder, so the only possible match is the owner handle folded into the lexical index text.
// TestLookupRendersCurrentHandleAfterRename: a lookup result must be runnable. The owner handle is
// part of the reference a caller (or sys/llm/decide) feeds straight back into run, so a rename has
// to show up immediately — not at the next restart, which is what an unbounded id→handle cache on
// the display path produced (§14 R8, §13 D15).
func TestLookupRendersCurrentHandleAfterRename(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)
	owner := setupUser(t, st, "rename-before", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "echo",
		Kind: kernel.KindHTTP, Active: false, Visibility: kernel.VisibilityPublic,
		Description:  "echo text back to the caller",
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
	// Warm the display cache the way ordinary traffic does, before the rename.
	if _, err := k.Lookup(ctx, kernel.LookupRequest{Query: "echo text", Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.RenameUser(ctx, sys.ID, owner.ID, "rename-after"); err != nil {
		t.Fatalf("RenameUser: %v", err)
	}

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "echo text", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Action != nil && r.Action.ID == a.ID {
			if r.OwnerHandle != "rename-after" {
				t.Fatalf("lookup rendered the stale handle %q; the reference would not resolve", r.OwnerHandle)
			}
			return
		}
	}
	t.Fatal("the action should still be found after its owner was renamed")
}

func TestLookupMatchesOwnerHandle(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "alice", 0)

	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "/translate",
		Kind: kernel.KindHTTP, Active: false, Visibility: kernel.VisibilityPublic,
		Description:  "convert text between languages",
		Source:       "https://example.com/api",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err != nil { // indexes via lookupText
		t.Fatalf("SetActive: %v", err)
	}

	results, err := k.Lookup(ctx, kernel.LookupRequest{Query: "alice", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !containsAction(results, a.ID) {
		t.Error("action should be found by its owner handle")
	}
}

// newTestKernelWithImportBPS builds a kernel over an existing store with a chosen origin import
// fee. Config is copied into the kernel at construction, so varying it means a second kernel over
// the same store — which is also the honest simulation of an operator restarting with new policy.
func newTestKernelWithImportBPS(st kernel.Store, importBPS int64) *kernel.Kernel {
	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = testIssuerUserID
	cfg.ImportBPS = importBPS
	return kernel.New(kernel.Dependencies{Store: st, Embedder: &fakeEmbedder{}, Config: cfg})
}

// TestLookupDiscoveredActionPrice: a discovered-but-unresolved remote action is priced from its
// signed serving price plus the ORIGIN's current import fee (§9, §13), so browsing needs no
// resolve. Changing import_bps reprices the same cached doc with no re-pull.
func TestLookupDiscoveredActionPrice(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// A local session caller: the discovery leg is offered only to a local authenticated user.
	caller := setupUser(t, st, "buyer", 0)

	// mp=100 at remote_bps=500 → serving price sr = 100 + ceil(100*500/10000) = 105.
	const servingPrice = int64(105)
	doc := &kernel.DiscoveryDoc{
		KernelPublicKey: "peer-key-1", Handle: "prov", ActionID: "act-remote", Name: "translate",
		Description:  "translate icelandic contracts",
		ServingPrice: servingPrice, ObservedAt: time.Now().UTC(),
	}
	if err := st.ReplaceDiscoveryDocs(ctx, "peer-key-1", []*kernel.DiscoveryDoc{doc}); err != nil {
		t.Fatalf("ReplaceDiscoveryDocs: %v", err)
	}

	find := func(k *kernel.Kernel) *kernel.LookupResult {
		t.Helper()
		res, err := k.Lookup(ctx, kernel.LookupRequest{Query: "translate icelandic", Limit: 10, CallerID: caller.ID})
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		for _, r := range res {
			if r.Discovered != nil && r.Discovered.ActionID == "act-remote" {
				return r
			}
		}
		t.Fatal("discovered remote action missing from lookup results")
		return nil
	}

	// import_bps = 500 → 105 + ceil(105*500/10000) = 105 + 6 = 111.
	if got := find(newTestKernelWithImportBPS(st, 500)).Price; got != 111 {
		t.Errorf("discovered price at import_bps=500 = %d, want 111", got)
	}
	// A policy change reprices the SAME cached doc, with no gossip re-pull:
	// import_bps = 2000 → 105 + ceil(105*2000/10000) = 105 + 21 = 126.
	if got := find(newTestKernelWithImportBPS(st, 2000)).Price; got != 126 {
		t.Errorf("discovered price at import_bps=2000 = %d, want 126", got)
	}

	// The stored serving price is untouched by either read.
	docs, err := st.ListDiscoveryDocs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].ServingPrice != servingPrice {
		t.Errorf("serving_price must round-trip unchanged, got %+v", docs)
	}
}

// TestLookupLocalActionPrice: price is returned for ordinary local hits too — one uniform field,
// not a remote special case (§9).
func TestLookupLocalActionPrice(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernelWithEmbedder(st, &fakeEmbedder{})
	ctx := context.Background()

	owner := setupUser(t, st, "provider", 0)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "forecast",
		Kind: kernel.KindHTTP, Active: true, Visibility: kernel.VisibilityPublic,
		Description: "weather forecast for a city", Price: 42,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	vec, _ := (&fakeEmbedder{}).Embed(ctx, a.Description)
	_ = st.UpsertEmbedding(ctx, a.ID, vec)

	res, err := k.Lookup(ctx, kernel.LookupRequest{Query: "weather forecast", Limit: 10})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(res) == 0 || res[0].Action == nil {
		t.Fatalf("expected a local hit, got %+v", res)
	}
	if res[0].Price != 42 {
		t.Errorf("local hit price = %d, want 42 (action.price)", res[0].Price)
	}
}
