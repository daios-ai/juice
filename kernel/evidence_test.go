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
	rating := kernel.Rating{ID: "r-" + receiptHash, Rating: val, RatedReceiptHash: receiptHash, CreatedAt: created}
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
	var uses int64
	for _, r := range rows {
		trade += r.RatingCount
		unver += r.UnverifiedRatings
		uses += r.Uses
	}
	if uses != 1 {
		t.Errorf("uses = %d, want 1 (only B's own execution row counts)", uses)
	}
	if trade != 1 {
		t.Errorf("trade-backed rating count = %d, want 1 (A only)", trade)
	}
	if unver != 1 {
		t.Errorf("unverified rating count = %d, want 1 (C's fabricated rating)", unver)
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
	sig, err := kernel.SignManifest(peerPriv, good)
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
		Users:           []kernel.GossipUser{{UserID: "u1", Handle: "prov", Description: "a provider"}},
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
