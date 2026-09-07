package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emptyStory is a story that did nothing, for the tests that judge a run with no evidence.
func emptyStory() *story {
	return &story{scale: 1, prices: map[string]int64{}}
}

func tx(id, parent, trace, status string, gross, fee, net, refund int64) map[string]any {
	return map[string]any{"id": id, "parent_trace_id": parent, "trace_id": trace,
		"status": status, "action_name": "a", "gross": float64(gross), "fee": float64(fee),
		"net": float64(net), "refund": float64(refund)}
}

// The refund law is the report's main claim, so it must be shown to reject the arithmetic it is
// meant to reject. Each case below fails if the check is removed or loosened.
func TestRefundLawAcceptsWhatBalancesAndRejectsWhatDoesNot(t *testing.T) {
	cases := []struct {
		name    string
		txs     []map[string]any
		wantBad int
	}{
		{"a plain successful call", []map[string]any{
			tx("t1", "", "T1", "success", 100, 20, 80, 0)}, 0},
		{"a failed call refunds everything", []map[string]any{
			tx("t1", "", "T1", "failure", 100, 0, 0, 100)}, 0},
		{"a parent that spent on one child", []map[string]any{
			tx("p", "", "P", "success", 100, 10, 30, 0),
			tx("c", "P", "C", "success", 60, 20, 40, 0)}, 0},
		// The case the suite exists to observe: the parent failed after one child had settled, so
		// it must refund its price less exactly what that child consumed.
		{"a partial refund", []map[string]any{
			tx("p", "", "P", "failure", 90, 0, 0, 65),
			tx("c1", "P", "C1", "success", 25, 5, 20, 0),
			tx("c2", "P", "C2", "failure", 11, 0, 0, 11)}, 0},
		{"a parent that refunded too much", []map[string]any{
			tx("p", "", "P", "failure", 90, 0, 0, 90),
			tx("c1", "P", "C1", "success", 25, 5, 20, 0)}, 1},
		{"a parent that refunded too little", []map[string]any{
			tx("p", "", "P", "failure", 90, 0, 0, 40),
			tx("c1", "P", "C1", "success", 25, 5, 20, 0)}, 1},
		{"a call that kept more than it was paid", []map[string]any{
			tx("t1", "", "T1", "success", 100, 60, 80, 0)}, 1},
		{"a failure that kept a fee", []map[string]any{
			tx("t1", "", "T1", "failure", 100, 20, 0, 100)}, 1},
	}
	for _, c := range cases {
		b, _, _, _ := refundLaw(map[string]Snapshot{"k": {Txs: c.txs}})
		if bad := len(b); bad != c.wantBad {
			t.Errorf("%s: found %d violations, want %d", c.name, bad, c.wantBad)
		}
	}
}

// A debt is one row on the serving side, so two kernels each recording the other as a debtor is a
// contradiction — and the earlier version of this check asserted the rows were opposites, which
// only ever passed when both were zero.
func TestContradictoryDebtsAreTheOnesDetected(t *testing.T) {
	ka := &Kernel{Name: "a", Key: "KA"}
	kb := &Kernel{Name: "b", Key: "KB"}
	sa := Snapshot{Peers: []map[string]any{{"public_key": "KB", "available": float64(-50)}}}
	sb := Snapshot{Peers: []map[string]any{{"public_key": "KA", "available": float64(0)}}}
	if peerBalance(sa, kb) != -50 {
		t.Error("a's row for b should show b owing 50")
	}
	if peerBalance(sb, ka) != 0 {
		t.Error("b's row for a should be zero; a debt is recorded once, on the serving side")
	}
	both := Snapshot{Peers: []map[string]any{{"public_key": "KA", "available": float64(-30)}}}
	if peerBalance(sa, kb) < 0 && peerBalance(both, ka) < 0 == false {
		t.Error("two kernels each recording the other as owing is the contradiction to catch")
	}
}

// A run that collected nothing must not report a pass. The check that enforces this is the one
// most likely to be quietly weakened, because a passing report is what everyone wants to see.
func TestAnEmptyRunCannotPass(t *testing.T) {
	n := &Net{Root: t.TempDir(), Kernels: map[string]*Kernel{},
		Expected: map[string]int{}, Unexpected: map[string]int{},
		Defects: map[string]string{}, Rail: playRail{}}
	f, err := openLog(n.Root)
	if err != nil {
		t.Fatal(err)
	}
	n.logFile = f
	defer f.Close()
	rep, err := Judge(n, emptyStory(), 1, map[string]any{})
	if err == nil {
		t.Fatal("a run with no kernels and no transactions reported a pass")
	}
	if rep.Verdicts["evidence_sufficient"] {
		t.Error("a run that collected nothing claimed sufficient evidence")
	}
	if !strings.Contains(err.Error(), "FAIL") {
		t.Errorf("the verdict should say FAIL, got %q", err)
	}
}

// An attack that never ran is not an attack that was refused. Counting a missing attack as a pass
// is how a campaign silently shrinks to nothing.
func TestAnAttackThatNeverRanCountsAgainstTheRun(t *testing.T) {
	n := &Net{Root: t.TempDir(), Kernels: map[string]*Kernel{},
		Expected: map[string]int{}, Unexpected: map[string]int{},
		Defects: map[string]string{}, Rail: playRail{}}
	f, _ := openLog(n.Root)
	n.logFile = f
	defer f.Close()
	rep, _ := Judge(n, emptyStory(), 1, map[string]any{})
	if rep.Verdicts["every_attack_refused"] {
		t.Error("a run that ran no attacks at all reported that every attack was refused")
	}
}

// A defect in the system under test and a fault in the harness need opposite responses, and
// keeping them in one list is how a real finding gets quietly resolved by adjusting the check that
// found it. Both must fail the run; only one must be reported as a product failure.
func TestAProductFailureIsReportedApartFromAHarnessFault(t *testing.T) {
	n := &Net{Root: t.TempDir(), Kernels: map[string]*Kernel{}, Expected: map[string]int{},
		Unexpected: map[string]int{}, Defects: map[string]string{}, Rail: playRail{}}
	f, _ := openLog(n.Root)
	n.logFile = f
	defer f.Close()

	n.Check("harness.check", false, "the check was written wrong")
	n.Product("money.stranded", "a killed provider left the caller's funds held")

	rep, err := Judge(n, emptyStory(), 1, map[string]any{})
	if err == nil {
		t.Fatal("a run with a product failure reported a pass")
	}
	if rep.Verdicts["no_product_failure_found"] {
		t.Error("a run that found a product failure claimed it had found none")
	}
	if rep.Verdicts["every_check_as_specified"] {
		t.Error("a product failure must also count as a check that did not behave as specified")
	}
	if len(rep.Product) != 1 || rep.Product["money.stranded"] == "" {
		t.Errorf("the product failure was not carried into the report: %v", rep.Product)
	}
	if _, isProduct := rep.Product["harness.check"]; isProduct {
		t.Error("a harness fault was recorded as a product failure")
	}

	body, err := os.ReadFile(filepath.Join(n.Root, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	md := string(body)
	if !strings.Contains(md, "Failures in the system under test") {
		t.Error("the report does not name the product failures as such")
	}
	if !strings.Contains(md, "a killed provider left the caller's funds held") {
		t.Error("the report does not say what the product failure was")
	}
	// The detail is what makes a finding survive: a label alone is easy to explain away.
	if strings.Contains(between(md, "Failures in the system under test", "###"), "harness.check") {
		t.Error("a harness fault was listed among the product failures")
	}
}

func acct(available, locked int64) map[string]any {
	return map[string]any{"available": float64(available), "locked": float64(locked)}
}

// Money is neither created nor destroyed. The identity holds with debts still open, which is what
// makes it worth checking at the end of a run that has them: a cross-kernel call sums to zero
// across the two kernels, and a settlement moves tokens between vaults without touching an account.
func TestConservationCatchesMoneyAppearingAndVanishing(t *testing.T) {
	deposits := Deposits{Total: 1000, By: map[string]int64{"k1": 600, "k2": 400}}
	balanced := map[string]Snapshot{
		"k1": {Users: []map[string]any{acct(500, 50), acct(20, 0)}}, // 570
		"k2": {Users: []map[string]any{acct(400, 30)}},              // 430
	}
	if in, held, _ := conservation(balanced, deposits); in != held {
		t.Errorf("a balanced economy was reported as %d in, %d held", in, held)
	}
	// Locked money is still money. Counting only what is available reports every reserved call as
	// a loss.
	ignoringLocked := map[string]Snapshot{
		"k1": {Users: []map[string]any{acct(570, 0)}},
		"k2": {Users: []map[string]any{acct(350, 80)}},
	}
	if in, held, _ := conservation(ignoringLocked, deposits); in != held {
		t.Errorf("locked funds were not counted: %d in, %d held", in, held)
	}
	created := map[string]Snapshot{"k1": {Users: []map[string]any{acct(2000, 0)}}}
	if in, held, _ := conservation(created, deposits); in == held {
		t.Error("an economy holding twice what entered it was reported as balanced")
	}
}

// A call must charge what its terms said. The remote case is the one that is easy to get wrong:
// a failed remote call may legitimately charge, because the peer did paid work before failing.
func TestPriceFidelityChecksLocalAndRemoteSeparately(t *testing.T) {
	// A proxy records the bare name of what it bought, so the test uses bare names throughout and
	// remoteness comes from where the action lives.
	prices := map[string]int64{"ana/echo": 10, "cara/quote": 25}
	owners := map[string]string{"ana/echo": "k1", "cara/quote": "k2"}
	rates := map[string]kernelRates{"k1": {remoteBps: 500, importBps: 500}, "k2": {remoteBps: 700, importBps: 300}}
	// Bought from k2 by k1: sr = 25 + ceil(25*7%) = 27; q = 27 + ceil(27*5%) = 29.
	remoteQuote := int64(29)

	// On k1: ana/echo is local, cara/quote is remote.
	ok := map[string]Snapshot{"k1": {Txs: []map[string]any{
		{"action_name": "ana/echo", "status": "success", "gross": float64(10), "fee": float64(2), "net": float64(8)},
		{"action_name": "cara/quote", "status": "success", "gross": float64(remoteQuote)},
		{"action_name": "ana/echo", "status": "failure", "gross": float64(10), "refund": float64(10)},
	}}}
	if b := priceFidelity(ok, prices, owners, rates, 1); len(b) != 0 {
		t.Errorf("correctly priced calls were reported as violations: %v", b)
	}

	wrong := map[string]Snapshot{"k1": {Txs: []map[string]any{
		{"action_name": "ana/echo", "status": "success", "gross": float64(12)},
	}}}
	if len(priceFidelity(wrong, prices, owners, rates, 1)) == 0 {
		t.Error("a call charged more than its price was accepted")
	}

	// A local failure keeps nothing.
	keptOnFailure := map[string]Snapshot{"k1": {Txs: []map[string]any{
		{"action_name": "ana/echo", "status": "failure", "gross": float64(10),
			"refund": float64(10), "fee": float64(2)},
	}}}
	if len(priceFidelity(keptOnFailure, prices, owners, rates, 1)) == 0 {
		t.Error("a failed local call that kept a fee was accepted")
	}

	// A failed remote call charges exactly what the receipt says the peer drew, plus the premium on
	// that draw, and no import fee. Demanding a full refund here would be wrong.
	receipt := `{"charge":20,"premium":2}`
	remoteFail := map[string]Snapshot{"k1": {Txs: []map[string]any{
		{"action_name": "cara/quote", "status": "failure", "gross": float64(remoteQuote),
			"refund": float64(remoteQuote - 22), "remote_receipt_json": receipt},
	}}}
	if b := priceFidelity(remoteFail, prices, owners, rates, 1); len(b) != 0 {
		t.Errorf("a failed remote call charging exactly its receipt's draw was rejected: %v", b)
	}
	overcharged := map[string]Snapshot{"k1": {Txs: []map[string]any{
		{"action_name": "cara/quote", "status": "failure", "gross": float64(remoteQuote),
			"refund": float64(0), "remote_receipt_json": receipt},
	}}}
	if len(priceFidelity(overcharged, prices, owners, rates, 1)) == 0 {
		t.Error("a failed remote call charging more than its receipt drew was accepted")
	}
	// A remote failure with no receipt has no justification for charging anything.
	noReceipt := map[string]Snapshot{"k1": {Txs: []map[string]any{
		{"action_name": "cara/quote", "status": "failure", "gross": float64(remoteQuote), "refund": float64(0)},
	}}}
	if len(priceFidelity(noReceipt, prices, owners, rates, 1)) == 0 {
		t.Error("a failed remote call charged with no receipt to justify it, and was accepted")
	}
}

// An obligation must be settled by a payment that is not short: a draw pays what is owed or the
// whole face value, never less. One that never closed is a break whatever it was for.
func TestSettlementFidelityChecksWhatWasPaid(t *testing.T) {
	exact := []settlement{{ID: "s1", Debtor: "k3", Creditor: "k2", Amount: 500, Obligation: 500, Closed: true}}
	if b := settlementFidelity(exact); len(b) != 0 {
		t.Errorf("an exact settlement was reported as a violation: %v", b)
	}
	won := []settlement{{ID: "s1", Debtor: "k3", Creditor: "k2", Amount: 10000, Obligation: 500, Closed: true}}
	if b := settlementFidelity(won); len(b) != 0 {
		t.Errorf("a won draw paying the whole face value was reported as a violation: %v", b)
	}
	short := []settlement{{ID: "s1", Debtor: "k3", Creditor: "k2", Amount: 400, Obligation: 500, Closed: true}}
	if len(settlementFidelity(short)) == 0 {
		t.Error("an obligation of 500 settled by a payment of 400 was accepted")
	}
	never := []settlement{{ID: "s1", Debtor: "k3", Creditor: "k2", Amount: 500, Obligation: 500}}
	if len(settlementFidelity(never)) == 0 {
		t.Error("an obligation that never closed was accepted")
	}
}

// An obligation is one row on the serving side, so it can be recorded on either kernel. Looking at
// only one direction hides everything owed to whichever name happens to sort second.
func TestOutstandingDebtIsFoundInBothDirections(t *testing.T) {
	ka, kb := &Kernel{Name: "aaa", Key: "KA"}, &Kernel{Name: "zzz", Key: "KB"}
	n := &Net{Root: t.TempDir(), Kernels: map[string]*Kernel{"aaa": ka, "zzz": kb},
		Expected: map[string]int{}, Unexpected: map[string]int{}, Defects: map[string]string{},
		Rail: playRail{}}
	f, _ := openLog(n.Root)
	n.logFile = f
	defer f.Close()
	// The obligation is recorded on zzz, the later name: zzz says aaa owes it for two calls.
	snaps := map[string]Snapshot{
		"aaa": {Peers: []map[string]any{{"public_key": "KB", "available": float64(0)}}},
		"zzz": {
			Peers: []map[string]any{{"public_key": "KA", "available": float64(0)}},
			Owed:  []map[string]any{{"id": "c1", "peer": "KA"}, {"id": "c2", "peer": "KA"}},
		},
	}
	if owedBy(snaps["zzz"], ka) != 2 {
		t.Fatal("the fixture does not record the debt where the test says it does")
	}
	contradictions, outstanding := positions(snaps, n.Kernels)
	if len(contradictions) != 0 {
		t.Errorf("rows holding nothing were reported as contradictions: %v", contradictions)
	}
	var found []string
	for _, o := range outstanding {
		found = append(found, strings.Fields(o)[0])
	}
	if len(found) != 1 || found[0] != "aaa" {
		t.Errorf("the debt owed by the earlier-sorting kernel was not found: %v", found)
	}
}

// A peer account is identity, attribution and moderation state — never a wallet. A row that holds
// anything at all is a contradiction under this economy, and the report must say so.
func TestAPeerRowHoldingMoneyIsAContradiction(t *testing.T) {
	ka, kb := &Kernel{Name: "aaa", Key: "KA"}, &Kernel{Name: "zzz", Key: "KB"}
	n := &Net{Root: t.TempDir(), Kernels: map[string]*Kernel{"aaa": ka, "zzz": kb}}
	snaps := map[string]Snapshot{
		"aaa": {Peers: []map[string]any{{"public_key": "KB", "available": float64(-750)}}},
		"zzz": {Peers: []map[string]any{{"public_key": "KA", "available": float64(0)}}},
	}
	contradictions, _ := positions(snaps, n.Kernels)
	if len(contradictions) != 1 {
		t.Errorf("a peer row holding -750 was not reported: %v", contradictions)
	}
}
