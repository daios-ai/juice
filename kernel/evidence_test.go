package kernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

	rows, err := k.SubjectEvidence(ctx, B)
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
	rows, _ := k.SubjectEvidence(ctx, B)
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
	rows, _ = k.SubjectEvidence(ctx, B)
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
	rows, err := k.SubjectEvidence(ctx, B)
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
	rows, _ = k.SubjectEvidence(ctx, B)
	for _, r := range rows {
		if r.IssuerPublicKey == A && r.SubjectActionID == "act2" && (r.RatingCount != 0 || r.UnverifiedRatings != 1 || r.CorroboratedUses != 0) {
			t.Errorf("a rating on another action's receipt counted as trade-backed: %+v", r)
		}
	}
	if err := st.UpsertEvidence(ctx, ratingEvidence(t, A, B, "act1", "H5", "H1", 0, now)); err != nil {
		t.Fatal(err)
	}
	rows, _ = k.SubjectEvidence(ctx, B)
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
	rows, _ = k.SubjectEvidence(ctx, B)
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

	// A manifest carrying someone else's (invalid) signature must be skipped.
	bad := &kernel.ActionManifest{
		ActionID: "act-bad", OwnerID: "u1", OwnerHandle: "prov", Name: "forged",
		Description: "should never be indexed", Kind: kernel.KindHTTP, Price: 5,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Signature: sig, // signature is for `good`, not `bad`
	}

	g := &kernel.GossipResponse{
		PublicKey: peerKey, Handle: "peerk",
		ActionManifests: []*kernel.ActionManifest{good, bad},
	}
	if _, err := k.AccumulateGossip(ctx, g, peerKey); err != nil {
		t.Fatalf("AccumulateGossip: %v", err)
	}

	docs, err := k.DiscoveryDocsForKernel(ctx, peerKey)
	if err != nil {
		t.Fatal(err)
	}
	var sawGood, sawBad bool
	for _, d := range docs {
		sawGood = sawGood || d.ActionID == "act-good"
		sawBad = sawBad || d.ActionID == "act-bad"
	}
	if !sawGood {
		t.Error("a validly-signed manifest must be indexed as a discovery doc")
	}
	if sawBad {
		t.Error("a badly-signed manifest must NOT be indexed")
	}
}
