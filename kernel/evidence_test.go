// SPDX-License-Identifier: AGPL-3.0-only

package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
)

func execEvidence(t *testing.T, issuer, subjKernel, action, counterparty, receiptHash string, created time.Time) *kernel.EvidenceRow {
	t.Helper()
	er := kernel.EvidenceReceipt{
		ReceiptHash: receiptHash, SubjectKernelPublicKey: subjKernel, SubjectActionID: action,
		CounterpartyKernelPublicKey: counterparty, Status: kernel.TxSuccess,
		StartedAt: created.Add(-100 * time.Millisecond), CreatedAt: created,
	}
	b, _ := json.Marshal(er)
	return &kernel.EvidenceRow{
		IssuerPublicKey: issuer, ReceiptHash: receiptHash, SubjectKernelPublicKey: subjKernel,
		SubjectActionID: action, CounterpartyKernelPublicKey: counterparty,
		EvidenceReceiptJSON: string(b), ReceiptCreatedAt: created, EffectiveAt: created, ObservedAt: created,
	}
}

func ratingEvidence(t *testing.T, issuer, subjKernel, action, receiptHash, remoteReceiptHash string, val float64, created time.Time) *kernel.EvidenceRow {
	t.Helper()
	er := kernel.EvidenceReceipt{
		ReceiptHash: receiptHash, SubjectKernelPublicKey: subjKernel, SubjectActionID: action,
		Status: kernel.TxSuccess, RemoteReceiptHash: remoteReceiptHash,
		StartedAt: created, CreatedAt: created,
	}
	erJSON, _ := json.Marshal(er)
	rating := kernel.RatingEvidence{Rating: val, RatedReceiptHash: receiptHash, CreatedAt: created}
	rJSON, _ := json.Marshal(rating)
	return &kernel.EvidenceRow{
		IssuerPublicKey: issuer, ReceiptHash: receiptHash, SubjectKernelPublicKey: subjKernel,
		SubjectActionID: action, EvidenceReceiptJSON: string(erJSON), RatingJSON: string(rJSON),
		RemoteReceiptHash: remoteReceiptHash, ReceiptCreatedAt: created, EffectiveAt: created, ObservedAt: created,
	}
}

// TestSubjectEvidenceTradeBackedBinding: a remote rating counts only when the two-kernel link holds —
// the subject's own execution evidence names the rater's kernel as counterparty and the hashes join.
// A rating fabricated against an observed receipt hash by a non-counterparty is retained but not counted.
func TestSubjectEvidenceTradeBackedBinding(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	now := time.Now().UTC()

	const B, A, C = "kernelB", "kernelA", "kernelC"
	// B's own execution evidence: A was the caller (counterparty), receipt hash H1.
	if err := st.UpsertEvidence(ctx, execEvidence(t, B, B, "act1", A, "H1", now)); err != nil {
		t.Fatal(err)
	}
	// A's genuine rating: references H1 as the remote receipt it settled on.
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act1", "H2", "H1", 1, now)); err != nil {
		t.Fatal(err)
	}
	// C's fabricated rating: it saw H1 in gossip and points at it, but C never traded with B.
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, C, B, "act1", "H3", "H1", 0, now)); err != nil {
		t.Fatal(err)
	}

	rows, err := k.SubjectEvidence(ctx, B, "")
	if err != nil {
		t.Fatal(err)
	}
	var trade, unver int64
	var subjectUses int64 // the execution summary counts only issuer==subject rows (§13 two views)
	for _, r := range rows {
		trade += r.RatingCount
		unver += r.UnverifiedRatings
		if r.IssuerPublicKey == B {
			subjectUses += r.Uses
		}
	}
	if subjectUses != 1 {
		t.Errorf("execution-summary uses = %d, want 1 (only B's own execution row, issuer==subject)", subjectUses)
	}
	if trade != 1 {
		t.Errorf("trade-backed rating count = %d, want 1 (A only)", trade)
	}
	if unver != 1 {
		t.Errorf("unverified rating count = %d, want 1 (C's fabricated rating)", unver)
	}
	// Interaction corroboration (not just ratings): A's interaction is trade-backed (B's own execution
	// names A as counterparty and H1 joins), C's is not (C never traded with B, so its claim is
	// uncorroborated even though it points at H1).
	for _, r := range rows {
		switch r.IssuerPublicKey {
		case A:
			if r.CorroboratedUses != 1 {
				t.Errorf("A's interaction should be corroborated: CorroboratedUses=%d, want 1", r.CorroboratedUses)
			}
		case C:
			if r.Uses != 1 || r.CorroboratedUses != 0 {
				t.Errorf("C's interaction is uncorroborated: uses=%d corroborated=%d, want 1 and 0", r.Uses, r.CorroboratedUses)
			}
		}
	}
}

// TestUpsertEvidenceLateRatingTransitions: a receipt gossiped without a rating, then the same receipt
// with a rating, ATTACHES the rating (not equivocation); two different ratings under one key equivocate.
func TestUpsertEvidenceLateRatingTransitions(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	now := time.Now().UTC()
	const B, A = "kB", "kA"

	if err := st.UpsertEvidence(ctx, execEvidence(t, B, B, "act1", A, "HX", now)); err != nil {
		t.Fatal(err)
	}
	// First: A's rating evidence with no rating yet (execution-only projection from A's side).
	bare := ratingEvidence(t, A, B, "act1", "HR", "HX", 1, now)
	bare.RatingJSON = "" // arrives without the rating
	if err := st.UpsertEvidence(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// Then: the same receipt WITH a rating attaches.
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act1", "HR", "HX", 1, now)); err != nil {
		t.Fatal(err)
	}
	rows, _ := k.SubjectEvidence(ctx, B, "")
	var count int64
	for _, r := range rows {
		count += r.RatingCount
	}
	if count != 1 {
		t.Fatalf("late rating must attach (count=%d, want 1)", count)
	}

	// A different rating value under the same (issuer, receipt_hash) → equivocation, both dropped.
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act1", "HR", "HX", 0, now)); err != nil {
		t.Fatal(err)
	}
	rows, _ = k.SubjectEvidence(ctx, B, "")
	count = 0
	for _, r := range rows {
		count += r.RatingCount
	}
	if count != 0 {
		t.Errorf("equivocated rating must not count (count=%d, want 0)", count)
	}
}

// TestSubjectEvidenceOneRatingPerTrade: an issuer's rows are unique on hashes it mints itself, so
// several can name one trade of the subject's. They are one rating when they agree, and none when
// they do not — one purchase never buys more than one rating.
func TestSubjectEvidenceOneRatingPerTrade(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	const A, B = "aaaa-issuer", "bbbb-subject"
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.UpsertEvidence(ctx, execEvidence(t, B, B, "act1", A, "H1", now)); err != nil {
		t.Fatal(err)
	}
	for _, own := range []string{"H2", "H3", "H4"} {
		if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act1", own, "H1", 1, now)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := k.SubjectEvidence(ctx, B, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.IssuerPublicKey == A && (r.RatingCount != 1 || r.Uses != 1 || r.CorroboratedUses != 1) {
			t.Errorf("three agreeing rows on one trade: want 1 rating, 1 use, 1 corroborated, got %d/%d/%d",
				r.RatingCount, r.Uses, r.CorroboratedUses)
		}
	}
	// The link is bound to the action: a rating for act2 naming act1's receipt is not trade-backed.
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act2", "H9", "H1", 1, now)); err != nil {
		t.Fatal(err)
	}
	rows, _ = k.SubjectEvidence(ctx, B, "")
	for _, r := range rows {
		if r.IssuerPublicKey == A && r.SubjectActionID == "act2" && (r.RatingCount != 0 || r.UnverifiedRatings != 1 || r.CorroboratedUses != 0) {
			t.Errorf("a rating on another action's receipt counted as trade-backed: %+v", r)
		}
	}
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act1", "H5", "H1", 0, now)); err != nil {
		t.Fatal(err)
	}
	rows, _ = k.SubjectEvidence(ctx, B, "")
	for _, r := range rows {
		if r.IssuerPublicKey == A && r.RatingCount != 0 {
			t.Errorf("a disagreeing row on the same trade: want 0 ratings, got %d", r.RatingCount)
		}
	}
	// An equivocated row (two ratings under one row key) voids its trade even beside honest,
	// agreeing rows: the issuer has told two stories about it.
	if err := st.UpsertEvidence(ctx, execEvidence(t, B, B, "act3", A, "H7", now)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act3", "H8", "H7", 1, now)); err != nil {
		t.Fatal(err)
	}
	for _, val := range []float64{1, 0} {
		if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act3", "H9e", "H7", val, now)); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ = k.SubjectEvidence(ctx, B, "")
	for _, r := range rows {
		if r.IssuerPublicKey == A && r.SubjectActionID == "act3" && r.RatingCount != 0 {
			t.Errorf("an equivocated sibling on the trade: want 0 ratings, got %d", r.RatingCount)
		}
	}
}

// TestAccumulateGossipIndexesVerifiedManifests: the receiver rebuilds a source kernel's discovery
// docs from ONLY its validly-signed first-party manifests, skipping any with a bad signature (§13).
func TestAccumulateGossipIndexesVerifiedManifests(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	peerPub, peerPriv, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(peerPub)

	good := &kernel.ActionManifest{
		ActionID: "act-good", OwnerID: "u1", OwnerHandle: "prov", Name: "translate",
		Description: "translate icelandic contracts", Kind: kernel.KindHTTP, Price: 5,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		UpdatedAt: time.Now().UTC(),
	}
	sig, err := testNet.SignManifest(peerPriv, good)
	if err != nil {
		t.Fatal(err)
	}
	good.Signature = sig

	// A manifest carrying someone else's signature fails the whole pull. The responder is
	// authenticated, so a manifest that does not verify is its fault: indexing the rest of the
	// page would keep whatever it chose to sign correctly and quietly drop the rest (P9).
	bad := &kernel.ActionManifest{
		ActionID: "act-bad", OwnerID: "u1", OwnerHandle: "prov", Name: "forged",
		Description: "should never be indexed", Kind: kernel.KindHTTP, Price: 5,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Signature: sig, // signature is for `good`, not `bad`
	}
	forged := &kernel.GossipResponse{
		NetworkDigest: testNet.Digest, PublicKey: peerKey, Handle: "peerk",
		ActionManifests: []*kernel.ActionManifest{good, bad},
	}
	if _, err := k.AccumulateGossip(ctx, forged, peerKey); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("a page carrying an unverifiable manifest must fail the pull, got %v", err)
	}
	if docs, _ := k.DiscoveryDocsForKernel(ctx, peerKey); len(docs) != 0 {
		t.Errorf("a failed pull must index nothing, got %d docs", len(docs))
	}

	g := &kernel.GossipResponse{
		NetworkDigest: testNet.Digest, PublicKey: peerKey, Handle: "peerk",
		ActionManifests: []*kernel.ActionManifest{good},
	}
	if _, err := k.AccumulateGossip(ctx, g, peerKey); err != nil {
		t.Fatalf("AccumulateGossip: %v", err)
	}

	docs, err := k.DiscoveryDocsForKernel(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	var sawGood bool
	for _, d := range docs {
		sawGood = sawGood || d.ActionID == "act-good"
	}
	if !sawGood {
		t.Error("a validly-signed manifest must be indexed as a discovery doc")
	}
}

// A buyer's account of a trade counts as corroborated only when the seller's own record of that
// same receipt says the same thing. A pair that links but disagrees is two signed statements that
// cannot both be true: it is counted as a contradiction and never as confirmation (§13, U39).
func TestCorroborationRequiresTheTwoStatementsToAgree(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	const seller, buyer, action = "SELLER", "BUYER", "act-1"

	// The seller's own record of two executions, each naming the buyer as the party it traded with.
	for _, c := range []struct {
		hash   string
		status kernel.TxStatus
	}{
		{"h-agree", kernel.TxSuccess},
		{"h-differ", kernel.TxSuccess},
	} {
		if err := st.UpsertEvidence(ctx, evidenceRow(t, seller, c.hash, seller, action, buyer, "", c.status)); err != nil {
			t.Fatal(err)
		}
	}
	// The buyer's account of the same two: one agrees, one does not.
	if err := st.UpsertEvidence(ctx, evidenceRow(t, buyer, "b-agree", seller, action, "", "h-agree", kernel.TxSuccess)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEvidence(ctx, evidenceRow(t, buyer, "b-differ", seller, action, "", "h-differ", kernel.TxFailure)); err != nil {
		t.Fatal(err)
	}

	rows, err := k.SubjectEvidence(ctx, seller, action)
	if err != nil {
		t.Fatal(err)
	}
	var buyerRow *kernel.SubjectEvidenceRow
	for _, r := range rows {
		if r.IssuerPublicKey == buyer {
			buyerRow = r
		}
	}
	if buyerRow == nil {
		t.Fatal("the buyer's account of the trades is missing")
	}
	if buyerRow.CorroboratedUses != 1 {
		t.Errorf("corroborated = %d, want 1: only the trade both sides describe the same way", buyerRow.CorroboratedUses)
	}
	if buyerRow.Contradictions != 1 {
		t.Errorf("contradictions = %d, want 1: the trade the two describe differently", buyerRow.Contradictions)
	}
	if buyerRow.Since.IsZero() || buyerRow.Until.IsZero() {
		t.Error("a row must state the window its counts cover")
	}
}

// One issuer, one receipt, two different statements: neither can be trusted and the trade counts
// for nothing. The stored statement is left exactly as it was signed — half-overwriting it would
// leave a row no signature covers (§13).
func TestASecondStatementAboutOneReceiptIsEquivocation(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	const seller, buyer, action = "SELLER2", "BUYER2", "act-2"

	if err := st.UpsertEvidence(ctx, evidenceRow(t, seller, "h-1", seller, action, buyer, "", kernel.TxSuccess)); err != nil {
		t.Fatal(err)
	}
	first := evidenceRow(t, buyer, "b-1", seller, action, "", "h-1", kernel.TxSuccess)
	if err := st.UpsertEvidence(ctx, first); err != nil {
		t.Fatal(err)
	}
	// The same receipt hash, a different story: a different counterparty and a different link.
	second := evidenceRow(t, buyer, "b-1", seller, action, "SOMEONE-ELSE", "h-other", kernel.TxFailure)
	if err := st.UpsertEvidence(ctx, second); err != nil {
		t.Fatal(err)
	}

	stored, err := st.ListEvidenceBySubject(ctx, seller, action)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range stored {
		if e.IssuerPublicKey != buyer {
			continue
		}
		if !e.Equivocated {
			t.Error("two statements about one receipt must mark the trade equivocated")
		}
		if e.EvidenceReceiptJSON != first.EvidenceReceiptJSON {
			t.Error("the stored statement must stay exactly as it was signed")
		}
		if e.CounterpartyKernelPublicKey == "SOMEONE-ELSE" || e.RemoteReceiptHash == "h-other" {
			t.Error("a second story must not re-aim the link the first one made")
		}
	}
	derived, derr := k.SubjectEvidence(ctx, seller, action)
	if derr != nil {
		t.Fatal(derr)
	}
	for _, r := range derived {
		if r.IssuerPublicKey == buyer && (r.Uses != 0 || r.CorroboratedUses != 0) {
			t.Errorf("an equivocated trade must count for nothing, got %+v", r)
		}
	}
}

// evidenceRow builds one stored evidence row the way ingest does: the signed projection as JSON,
// plus the columns the joins read.
func evidenceRow(t *testing.T, issuer, receiptHash, subjectKernel, subjectAction, counterparty, remoteHash string, status kernel.TxStatus) *kernel.EvidenceRow {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	er := kernel.EvidenceReceipt{
		ReceiptHash: receiptHash, SubjectKernelPublicKey: subjectKernel, SubjectActionID: subjectAction,
		CounterpartyKernelPublicKey: counterparty, RemoteReceiptHash: remoteHash,
		Status: status, StartedAt: now, CreatedAt: now,
	}
	b, err := json.Marshal(er)
	if err != nil {
		t.Fatal(err)
	}
	return &kernel.EvidenceRow{
		IssuerPublicKey: issuer, ReceiptHash: receiptHash,
		SubjectKernelPublicKey: subjectKernel, SubjectActionID: subjectAction,
		CounterpartyKernelPublicKey: counterparty, RemoteReceiptHash: remoteHash,
		EvidenceReceiptJSON: string(b),
		ReceiptCreatedAt:    now, EffectiveAt: now, ObservedAt: now,
	}
}

// A catalogue arrives a page at a time and is marked into a scan generation, never replaced. Until
// a scan completes, what it has not mentioned yet is still held: a large catalogue would otherwise
// lose its first page before its last arrived, and a page that happened to be empty would erase
// everything known about the peer. Only the page that ends the catalogue sweeps what the scan did
// not mention (P9, U40).
func TestACatalogueSweepsOnlyWhenItsScanCompletes(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	peerPub, peerPriv, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(peerPub)

	manifest := func(id string) *kernel.ActionManifest {
		m := &kernel.ActionManifest{
			ActionID: id, OwnerID: "u1", OwnerHandle: "prov", Name: id, Description: "d " + id,
			Kind: kernel.KindHTTP, Price: 5,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			UpdatedAt: time.Now().UTC(),
		}
		sig, err := testNet.SignManifest(peerPriv, m)
		if err != nil {
			t.Fatal(err)
		}
		m.Signature = sig
		return m
	}
	page := func(next string, ids ...string) *kernel.GossipResponse {
		g := &kernel.GossipResponse{NetworkDigest: testNet.Digest, PublicKey: peerKey, Handle: "peerk",
			NextCatalogCursor: next}
		for _, id := range ids {
			g.ActionManifests = append(g.ActionManifests, manifest(id))
		}
		return g
	}
	held := func() map[string]bool {
		docs, err := k.DiscoveryDocsForKernel(ctx, peerKey)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, d := range docs {
			out[d.ActionID] = true
		}
		return out
	}

	// A first, complete scan: three actions in one page that ends the catalogue.
	if _, err := k.AccumulateGossip(ctx, page("", "a", "b", "c"), peerKey); err != nil {
		t.Fatal(err)
	}
	if got := held(); !got["a"] || !got["b"] || !got["c"] {
		t.Fatalf("after a complete scan the catalogue is %v, want a, b and c", got)
	}

	// A second scan in which the peer no longer serves c. Its first page mentions only a: nothing
	// may be removed yet, because the rest of the catalogue has not arrived.
	if _, err := k.AccumulateGossip(ctx, page("a", "a"), peerKey); err != nil {
		t.Fatal(err)
	}
	if got := held(); !got["b"] || !got["c"] {
		t.Errorf("mid-scan the catalogue is %v, want b and c still held", got)
	}

	// The page that ends the catalogue completes the scan, and only then is what the scan never
	// mentioned removed.
	if _, err := k.AccumulateGossip(ctx, page("", "b"), peerKey); err != nil {
		t.Fatal(err)
	}
	got := held()
	if !got["a"] || !got["b"] {
		t.Errorf("a completed scan must keep what it mentioned, got %v", got)
	}
	if got["c"] {
		t.Error("a completed scan must remove what the peer no longer serves")
	}
}

// The evidence this kernel collects is for the buyer deciding whether to trust a provider, so it
// must be readable where the buyer looks: on the action itself. A proxy's subject is the peer that
// runs it and the id it runs under, so the provider's own record and other kernels' accounts of
// the same trades appear side by side, and a rating given on another kernel is admitted here
// through the link to the provider's own receipt (§13, U39, D11).
func TestABuyerSeesTheEvidenceAboutARemoteAction(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	providerPub, providerPriv, _ := ed25519.GenerateKey(rand.Reader)
	providerKey := base64.RawURLEncoding.EncodeToString(providerPub)
	const otherBuyer, remoteAction = "kernelM", "remote-1"

	k := newTestKernel(st)
	setupSys(t, k, st)
	provider, err := k.EnsureKernelAccount(ctx, providerKey)
	if err != nil {
		t.Fatal(err)
	}
	bindPetnameForTest(t, k, ctx, providerKey, "provider")
	m := kernel.ActionManifest{
		ActionID: remoteAction, OwnerHandle: "provider", Name: "translate", Kind: kernel.KindHTTP,
		Price: 10, RemoteBPS: kernel.DefaultEconomy().RemoteBPS, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		UpdatedAt: time.Now().UTC(),
	}
	m.Signature, _ = testNet.SignManifest(providerPriv, &m)
	proxy, err := k.ImportPeerAction(ctx, provider.ID, m)
	if err != nil {
		t.Fatal(err)
	}

	// What the two kernels gossiped: the provider's own record of a trade with M, and M's account
	// of that same trade, carrying its rating.
	now := time.Now().UTC()
	if err := st.UpsertEvidence(ctx, execEvidence(t, providerKey, providerKey, remoteAction, otherBuyer, "H1", now)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, otherBuyer, providerKey, remoteAction, "H2", "H1", 1, now)); err != nil {
		t.Fatal(err)
	}

	record := k.ActionRecord(ctx, proxy)
	if record.ProviderReported == nil {
		t.Fatal("a buyer must see the provider's own report about the action it is selling")
	}
	var other *kernel.SubjectEvidenceRow
	for _, r := range record.ObservedByOthers {
		if r.IssuerPublicKey == otherBuyer {
			other = r
		}
	}
	if other == nil {
		t.Fatal("a buyer must see what other kernels report about the same action")
	}
	if other.CorroboratedUses != 1 {
		t.Errorf("M's trade is confirmed by the provider's own record: corroborated=%d, want 1",
			other.CorroboratedUses)
	}
	if other.RatingCount != 1 {
		t.Errorf("M's rating is backed by that trade: count=%d, want 1", other.RatingCount)
	}

	// The same holds for the ratings projection: a rating paid for abroad belongs to the
	// provider's track record as much as one paid for here.
	ratings, err := k.ActionRatings(ctx, proxy, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ratings) != 1 {
		t.Fatalf("a proxy's ratings = %d, want the one trade-backed peer rating", len(ratings))
	}
	if ratings[0].Source != "peer" {
		t.Errorf("rating source = %q, want it marked as another kernel's", ratings[0].Source)
	}
}

// A party that tells two stories about one trade is detected, and the detection is shown: the
// trade counts for nothing, and the count of such trades is on the row. Without it a reader
// cannot tell a kernel that contradicts itself from one that never traded at all (U39, D16).
func TestAnIssuerTellingTwoStoriesIsCountedWhereItIsRead(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	now := time.Now().UTC()
	const seller, liar, action = "SELLER", "LIAR", "act-1"

	// The seller's own record of the trade, naming the liar as the party it traded with.
	if err := st.UpsertEvidence(ctx, execEvidence(t, seller, seller, action, liar, "H1", now)); err != nil {
		t.Fatal(err)
	}
	// The liar's account of that same trade, then a second, different signed account of it.
	first := ratingEvidence(t, liar, seller, action, "H2", "H1", 1, now)
	if err := st.UpsertEvidence(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := ratingEvidence(t, liar, seller, action, "H2", "H1", 0, now)
	if err := st.UpsertEvidence(ctx, second); err != nil {
		t.Fatal(err)
	}

	told := issuerRow(t, k, ctx, seller, action, liar)
	if told.Equivocations != 1 {
		t.Errorf("equivocations = %d, want the one trade told two ways", told.Equivocations)
	}
	if told.Uses != 0 || told.RatingCount != 0 || told.UnverifiedRatings != 0 {
		t.Errorf("a trade told two ways must count for nothing else: uses=%d rated=%d unverified=%d",
			told.Uses, told.RatingCount, told.UnverifiedRatings)
	}
}

// The same rule holds when the two stories are two rows rather than one row written twice: an
// issuer can write about one trade under several receipt hashes of its own, and a reader's
// question is whether that party can be believed, not which row it used to contradict itself.
func TestTwoRowsDisagreeingAboutOneTradeAreEquivocationToo(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	now := time.Now().UTC()
	const seller, buyer, action = "SELLER", "BUYER", "act-1"

	if err := st.UpsertEvidence(ctx, execEvidence(t, seller, seller, action, buyer, "H1", now)); err != nil {
		t.Fatal(err)
	}
	// Two rows of the buyer's, about the same trade (same remote receipt), under its own two
	// hashes: one says the call succeeded, the other that it failed.
	good := ratingEvidence(t, buyer, seller, action, "H2", "H1", 1, now)
	if err := st.UpsertEvidence(ctx, good); err != nil {
		t.Fatal(err)
	}
	bad := ratingEvidence(t, buyer, seller, action, "H3", "H1", 1, now)
	bad.EvidenceReceiptJSON = strings.Replace(bad.EvidenceReceiptJSON, `"status":"success"`, `"status":"failure"`, 1)
	if err := st.UpsertEvidence(ctx, bad); err != nil {
		t.Fatal(err)
	}

	told := issuerRow(t, k, ctx, seller, action, buyer)
	if told.Equivocations != 1 {
		t.Errorf("equivocations = %d, want the trade this issuer told two ways", told.Equivocations)
	}
	if told.Uses != 0 || told.CorroboratedUses != 0 || told.RatingCount != 0 {
		t.Errorf("a trade told two ways must count for nothing else: uses=%d corroborated=%d rated=%d",
			told.Uses, told.CorroboratedUses, told.RatingCount)
	}
}

// issuerRow is one issuer's row of the derived view, which must exist even when everything it
// says is contradicted: a reader told nothing cannot tell silence from a contradiction.
func issuerRow(t *testing.T, k *kernel.Kernel, ctx context.Context, subjectKernel, action, issuer string) *kernel.SubjectEvidenceRow {
	t.Helper()
	rows, err := k.SubjectEvidence(ctx, subjectKernel, action)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.IssuerPublicKey == issuer {
			return r
		}
	}
	t.Fatalf("issuer %s is absent from the view entirely", issuer)
	return nil
}

// A page is the size this protocol serves. A peer sending more is not offering more: it is asking
// this kernel to verify signatures and embed descriptions by the thousand off one frame, and the
// byte limit is no bound on that because small objects are many. The cardinality is checked before
// any per-item work, and an oversized page is refused whole rather than truncated — truncating
// would drop items the cursor then moves past (P9, D12).
func TestAGossipPageOverTheProtocolsSizeIsRefusedWhole(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	peerPub, peerPriv, _ := ed25519.GenerateKey(rand.Reader)
	peerKey := base64.RawURLEncoding.EncodeToString(peerPub)

	manifest := func(id string) *kernel.ActionManifest {
		m := &kernel.ActionManifest{
			ActionID: id, OwnerID: "u1", OwnerHandle: "prov", Name: id, Description: "d",
			Kind: kernel.KindHTTP, Price: 5,
			InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
			UpdatedAt: time.Now().UTC(),
		}
		sig, err := testNet.SignManifest(peerPriv, m)
		if err != nil {
			t.Fatal(err)
		}
		m.Signature = sig
		return m
	}
	g := &kernel.GossipResponse{NetworkDigest: testNet.Digest, PublicKey: peerKey, Handle: "peerk"}
	for i := 0; i <= kernel.CatalogPageSizeForTest; i++ { // one over the page this protocol serves
		g.ActionManifests = append(g.ActionManifests, manifest(fmt.Sprintf("a%03d", i)))
	}
	if _, err := k.AccumulateGossip(ctx, g, peerKey); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("an oversized page = %v, want it refused", err)
	}
	if docs, _ := k.DiscoveryDocsForKernel(ctx, peerKey); len(docs) != 0 {
		t.Errorf("a refused page indexed %d documents; it must be refused whole", len(docs))
	}
}

// The two surfaces a buyer reads — the action's record and its ratings — answer with one
// predicate. A rating whose trade the issuer told two ways is admitted by neither: if the ratings
// query were the weaker rule, `action show` would report a contradiction while `action ratings`
// published the rating it came with (D11, D16).
func TestTheRatingsSurfaceAndTheActionRecordAgree(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	now := time.Now().UTC()
	providerPub, providerPriv, _ := ed25519.GenerateKey(rand.Reader)
	providerKey := base64.RawURLEncoding.EncodeToString(providerPub)
	const buyer, remoteAction = "BUYER", "remote-1"

	setupSys(t, k, st)
	provider, err := k.EnsureKernelAccount(ctx, providerKey)
	if err != nil {
		t.Fatal(err)
	}
	bindPetnameForTest(t, k, ctx, providerKey, "provider")
	m := kernel.ActionManifest{
		ActionID: remoteAction, OwnerHandle: "provider", Name: "translate", Kind: kernel.KindHTTP,
		Price: 10, RemoteBPS: kernel.DefaultEconomy().RemoteBPS, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		UpdatedAt: now,
	}
	m.Signature, _ = testNet.SignManifest(providerPriv, &m)
	proxy, err := k.ImportPeerAction(ctx, provider.ID, m)
	if err != nil {
		t.Fatal(err)
	}

	// The provider's own record of the trade, and two rows of the buyer's about it: same rating,
	// opposite outcomes.
	if err := st.UpsertEvidence(ctx, execEvidence(t, providerKey, providerKey, remoteAction, buyer, "H1", now)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, buyer, providerKey, remoteAction, "H2", "H1", 1, now)); err != nil {
		t.Fatal(err)
	}
	disagrees := ratingEvidence(t, buyer, providerKey, remoteAction, "H3", "H1", 1, now)
	disagrees.EvidenceReceiptJSON = strings.Replace(disagrees.EvidenceReceiptJSON, `"status":"success"`, `"status":"failure"`, 1)
	if err := st.UpsertEvidence(ctx, disagrees); err != nil {
		t.Fatal(err)
	}

	record := k.ActionRecord(ctx, proxy)
	counted := int64(0)
	for _, r := range record.ObservedByOthers {
		counted += r.RatingCount
	}
	ratings, err := k.ActionRatings(ctx, proxy, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if counted != 0 || len(ratings) != 0 {
		t.Errorf("the record counted %d ratings and the ratings surface published %d; one trade told two ways is admitted by neither",
			counted, len(ratings))
	}
}

// A trade is named by the whole tuple — subject kernel, subject action, receipt, counterparty —
// because an action id is unique only inside the kernel that issued it. A provider's record about
// some other kernel's action, carrying the same id, corroborates nothing here (D11, D16).
func TestCorroborationNamesTheSubjectKernelNotJustTheAction(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	now := time.Now().UTC()
	providerPub, providerPriv, _ := ed25519.GenerateKey(rand.Reader)
	providerKey := base64.RawURLEncoding.EncodeToString(providerPub)
	const buyer, elsewhere, action = "BUYER", "OTHER-KERNEL", "act-1"

	setupSys(t, k, st)
	provider, err := k.EnsureKernelAccount(ctx, providerKey)
	if err != nil {
		t.Fatal(err)
	}
	bindPetnameForTest(t, k, ctx, providerKey, "provider")
	m := kernel.ActionManifest{
		ActionID: action, OwnerHandle: "provider", Name: "translate", Kind: kernel.KindHTTP,
		Price: 10, RemoteBPS: kernel.DefaultEconomy().RemoteBPS, Description: "d",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		UpdatedAt: now,
	}
	m.Signature, _ = testNet.SignManifest(providerPriv, &m)
	proxy, err := k.ImportPeerAction(ctx, provider.ID, m)
	if err != nil {
		t.Fatal(err)
	}

	// The provider's only record naming this receipt is about an action on ANOTHER kernel that
	// happens to carry the same id. The buyer's rating is about the provider's action.
	if err := st.UpsertEvidence(ctx, execEvidence(t, providerKey, elsewhere, action, buyer, "H1", now)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, buyer, providerKey, action, "H2", "H1", 1, now)); err != nil {
		t.Fatal(err)
	}

	ratings, err := k.ActionRatings(ctx, proxy, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ratings) != 0 {
		t.Errorf("a record about another kernel's action corroborated a rating about this one: %+v", ratings)
	}
	// And the action's own read says the same: the claim is there, unverified, not confirmed.
	record := k.ActionRecord(ctx, proxy)
	for _, r := range record.ObservedByOthers {
		if r.IssuerPublicKey == buyer && r.RatingCount != 0 {
			t.Errorf("the record counted %d trade-backed ratings on an unlinked claim", r.RatingCount)
		}
	}
}
