package kernel

import (
	"encoding/hex"
	"testing"
)

// The domestic fee splits a taxable amount without losing or minting a unit, whatever the rate.
func TestFeeSplitsExactly(t *testing.T) {
	for _, c := range []struct {
		taxable, bps, wantFee int64
	}{
		{0, 2000, 0},    // nothing to tax
		{100, 0, 0},     // no fee configured
		{100, 2000, 20}, // the ordinary case
		{1, 2000, 1},    // rounds up, so a fee is never silently waived
		{99, 3333, 33},  // ceil(32.99…)
		{100, 10000, 100}} {
		e := Economy{FeeBPS: c.bps}
		net, fee := e.Fee(c.taxable)
		if fee != c.wantFee {
			t.Errorf("Fee(%d) at %d bps = %d, want %d", c.taxable, c.bps, fee, c.wantFee)
		}
		if net+fee != c.taxable {
			t.Errorf("net %d + fee %d != taxable %d", net, fee, c.taxable)
		}
	}
}

// The two markups price a cross-kernel call in the order the protocol applies them: the seller's
// rate over its own price, then the buying kernel's over that.
func TestServingAndLocalPrice(t *testing.T) {
	e := Economy{ImportBPS: 1000}
	sr, err := e.ServingPrice(25, 700)
	if err != nil || sr != 27 { // 25 + ceil(25·7%) = 25 + 2
		t.Fatalf("ServingPrice(25, 700) = %d, %v; want 27", sr, err)
	}
	q, err := e.LocalPrice(sr)
	if err != nil || q != 30 { // 27 + ceil(27·10%) = 27 + 3
		t.Fatalf("LocalPrice(27) = %d, %v; want 30", q, err)
	}
	if _, err := e.ServingPrice(-1, 700); err == nil {
		t.Error("a negative price must be refused")
	}
	if _, err := e.ServingPrice(25, 10001); err == nil {
		t.Error("a rate above 100% must be refused")
	}
}

// The stake is the whole face value whenever a draw is possible at all, and nothing when it is not.
//
// Two things rule out the smaller stakes one might reach for. It is not L − D, because the call's
// budget returns the obligation to the caller at commit and the payment is then taken whole. And it
// is not "zero when D_max ≥ L", because what is drawn on is the actual obligation, which can come in
// anywhere below D_max: a call advertised above the face value can still settle beneath it and win,
// and a caller who staked nothing would then be short.
func TestStakeIsTheWholeFaceValue(t *testing.T) {
	e := Economy{Lottery: 100}
	for _, c := range []struct{ dmax, want int64 }{
		{0, 0},     // a free call owes nothing, so nothing can be drawn
		{1, 100},   // the lottery applies
		{99, 100},  //
		{100, 100}, // the advertised price is at the face value, but what settles may be below it
		{250, 100}, //
	} {
		if got := e.Stake(c.dmax); got != c.want {
			t.Errorf("Stake(%d) at L=100 = %d, want %d", c.dmax, got, c.want)
		}
	}
	if got := (Economy{Lottery: 0}).Stake(50); got != 0 {
		t.Errorf("with no lottery every obligation is exact, so the stake is 0, got %d", got)
	}
}

// The draw pays the face value with probability d/L, so the expected payment is the obligation —
// which is the whole point of settling small amounts this way. It also pays exactly at or above the
// face value, and pays nothing for a zero obligation.
func TestDrawPaysTheObligationInExpectation(t *testing.T) {
	secret, _ := hex.DecodeString("aabb")
	if got := Draw("t1", secret, []byte("n"), 0, 100); got != 0 {
		t.Errorf("a zero obligation drew %d", got)
	}
	if got := Draw("t1", secret, []byte("n"), 40, 0); got != 40 {
		t.Errorf("with no lottery the obligation is paid exactly, got %d", got)
	}
	if got := Draw("t1", secret, []byte("n"), 150, 100); got != 150 {
		t.Errorf("an obligation above the face value is paid exactly, got %d", got)
	}
	const L, d, N = int64(100), int64(25), 40000
	var total int64
	for i := 0; i < N; i++ {
		total += Draw("t1", secret, []byte{byte(i), byte(i >> 8)}, d, L)
	}
	mean := float64(total) / N
	if mean < 0.9*float64(d) || mean > 1.1*float64(d) {
		t.Errorf("mean payment %.2f over %d draws, want ≈%d", mean, N, d)
	}
}

// Both kernels draw from the same three values and must reach the same answer, and changing any one
// of them changes the answer — so neither side can quietly substitute its own.
func TestDrawIsBoundToItsThreeInputs(t *testing.T) {
	base := Draw("t1", []byte("s"), []byte("n"), 50, 100)
	if base != Draw("t1", []byte("s"), []byte("n"), 50, 100) {
		t.Fatal("the two sides would disagree")
	}
	differs := 0
	for _, alt := range []int64{
		Draw("t2", []byte("s"), []byte("n"), 50, 100),
		Draw("t1", []byte("z"), []byte("n"), 50, 100),
		Draw("t1", []byte("s"), []byte("z"), 50, 100),
	} {
		if alt != base {
			differs++
		}
	}
	if differs == 0 {
		t.Error("changing the ticket, the secret or the nonce moved nothing")
	}
}

// TestRemoteSettlement pins the one calculation both settlement and the offline audit run (P7): a
// disagreement between them is what the shared function exists to make impossible.
func TestRemoteSettlement(t *testing.T) {
	e := Economy{RemoteBPS: 500, ImportBPS: 500}
	for _, tc := range []struct {
		name                          string
		charge, premium               int64
		success                       bool
		wantObligation, wantImportFee int64
		wantOK                        bool
	}{
		{"success", 1000, 50, true, 1050, 53, true},
		{"failure charges the premium but retains no import fee", 1000, 50, false, 1050, 0, true},
		{"a failure that consumed nothing owes nothing", 0, 0, false, 0, 0, true},
		{"a premium the rates do not produce is refused", 1000, 60, true, 1060, 53, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obligation, importFee, ok := e.RemoteSettlement(tc.charge, tc.premium, e.RemoteBPS, e.ImportBPS, tc.success)
			if obligation != tc.wantObligation || importFee != tc.wantImportFee || ok != tc.wantOK {
				t.Errorf("got (%d, %d, %v), want (%d, %d, %v)",
					obligation, importFee, ok, tc.wantObligation, tc.wantImportFee, tc.wantOK)
			}
		})
	}
	// The rates are the call's own, not the live fields: a settlement is audited at what it was sold
	// at, so a later config change must not move it.
	moved := Economy{RemoteBPS: 9000, ImportBPS: 9000}
	if _, fee, ok := moved.RemoteSettlement(1000, 50, 500, 500, true); fee != 53 || !ok {
		t.Errorf("frozen rates ignored: fee=%d ok=%v, want 53 and true", fee, ok)
	}
}
