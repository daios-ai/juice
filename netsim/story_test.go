// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The rule this suite is built on: one economy, run unchanged on every rail. It fails silently —
// the reports still look comparable while describing different economies — so it is enforced here
// rather than left to review. The story may ask the rail for money, payment and finality; it may
// not ask which rail it is.
func TestTheStoryDoesNotKnowWhichRailItIsOn(t *testing.T) {
	src, err := os.ReadFile("story.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := []struct {
		pattern, why string
	}{
		{`"play"`, "the story names a rail"},
		{`"anvil"`, "the story names a rail"},
		{`"sepolia"`, "the story names a rail"},
		{`Rail\.Name\(\)`, "the story asks which rail it is on"},
	}
	for _, b := range banned {
		if m := regexp.MustCompile(b.pattern).Find(src); m != nil {
			t.Errorf("%s: found %q in story.go. Participants, actions, prices, trades, "+
				"compositions, attacks and assertions are the same on every rail; only funding, "+
				"payment, finality and measurement belong to the Rail", b.why, m)
		}
	}
}

// The rail may vary how often the trading rounds repeat, and nothing else. If a rail could vary
// anything else, the same guard above would have to be re-read every time the interface grew.
func TestTheRailInterfaceOffersNoStoryChoices(t *testing.T) {
	src, err := os.ReadFile("rail.go")
	if err != nil {
		t.Fatal(err)
	}
	iface := between(string(src), "type Rail interface {", "\n}")
	allowed := map[string]bool{
		"Name": true, "Prepare": true, "Config": true, "Scale": true, "GasUp": true,
		"Fund": true, "Credit": true, "SettleWait": true, "Finish": true,
	}
	for _, line := range strings.Split(iface, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		name := strings.SplitN(line, "(", 2)[0]
		if !allowed[name] {
			t.Errorf("the Rail interface grew a method %q. A rail may supply money, payment, "+
				"finality and measurement; anything else lets a world change the economy", name)
		}
	}
}

// The story's declared shape is what a budgeted rail prices before it spends anything. If it drifts
// from the trades the story actually makes, a rail can approve a run it cannot pay for.
func TestShapeMatchesTheTradesTheStoryMakes(t *testing.T) {
	s := StoryShape(12)
	if s.Kernels != len(kernelPlan) {
		t.Errorf("shape says %d kernels, the plan has %d", s.Kernels, len(kernelPlan))
	}
	// Only the users who bring money in from outside cost a chain rail anything.
	payers, receivers := 0, 0
	for _, u := range userPlan {
		if u.bringsIn > 0 {
			payers++
		}
		if u.receives > 0 {
			receivers++
		}
	}
	// Every account funded from outside costs a chain rail a mint and a transfer: the users the
	// plan funds, and the attackers, who must pay their own way or they never reach the cap.
	if s.PayingUsers != payers+len(sybilPlan) {
		t.Errorf("shape says %d paying users; %d users bring money in and %d attackers fund themselves",
			s.PayingUsers, payers, len(sybilPlan))
	}
	// Every kernel that holds money must have exactly one point of entry, or a chain rail funds
	// one account twice and the other not at all.
	entries := map[string]int{}
	for _, u := range userPlan {
		if u.bringsIn > 0 {
			entries[u.on]++
		}
	}
	for _, k := range kernelPlan {
		if entries[k.name] != 1 {
			t.Errorf("kernel %s has %d points of entry for money; it must have exactly one, "+
				"because a payout address belongs to one account", k.name, entries[k.name])
		}
	}
	// Everyone who receives must have someone on their own kernel to receive from.
	for _, u := range userPlan {
		if u.receives > 0 && entries[u.on] == 0 {
			t.Errorf("%s on %s is funded by transfer, but nobody pays into that kernel", u.handle, u.on)
		}
	}
	if receivers == 0 {
		t.Error("no user is funded by a local transfer, so that path is never exercised")
	}
	// Every kernel that buys across a boundary in ANY act will settle, not only those in the
	// trading rounds. A kernel left out of this count is a kernel left without gas.
	debtors := map[string]bool{}
	for _, tr := range crossKernelTrades {
		debtors[tr.kernel] = true
	}
	for _, tr := range otherCrossKernelTrades {
		debtors[tr.kernel] = true
	}
	if s.SigningKernels != len(debtors) {
		t.Errorf("shape says %d signing kernels, but %d kernels buy across a boundary",
			s.SigningKernels, len(debtors))
	}
	// The acts outside the trading rounds must be declared, or a chain rail is not told about the
	// payments they cause and runs out of gas partway through.
	for _, tr := range otherCrossKernelTrades {
		if s.PaymentsPerKernel[tr.kernel] == 0 {
			t.Errorf("kernel %s trades across a boundary in a later act but is budgeted no payment",
				tr.kernel)
		}
	}
	if s.SigningKernels == 0 || s.PayingUsers == 0 {
		t.Fatal("a shape with no payers or no signers would let any budget approve any run")
	}
}

// Every cross-kernel call that owes draws its own ticket, and every winning draw is its own
// payment (P10), so the number of payments grows with the rounds. Counting the ordered pairs the
// trade graph forms — one per creditor, whatever the volume — funded a kernel for two payments
// where it made thirty-three, and it stalled the moment its vault ran dry.
func TestPaymentsAreCountedPerCallNotPerCounterparty(t *testing.T) {
	few, many := StoryShape(1), StoryShape(12)
	for k, n := range few.PaymentsPerKernel {
		if k == "k4" || k == "k1" { // the kernels the trading rounds actually make buy
			if many.PaymentsPerKernel[k] <= n {
				t.Errorf("kernel %s is budgeted %d payments over 12 rounds and %d over one; "+
					"more trading must cost more payments", k, many.PaymentsPerKernel[k], n)
			}
		}
	}
	// A kernel that buys in the acts outside the rounds is budgeted for those too, whatever the
	// number of rounds: the burst alone is eighty calls.
	if StoryShape(1).PaymentsPerKernel["k4"] < 10 {
		t.Errorf("k4 makes eighty burst calls beyond the rounds but is budgeted %d payments",
			StoryShape(1).PaymentsPerKernel["k4"])
	}
	// A free action owes nothing, so it can never cost a payment.
	if odds := payingOdds("gus/index", "k4"); odds != 0 {
		t.Errorf("a free action was priced at %v of a payment", odds)
	}
	// An obligation at or above the face value is paid in full every time; one below it is paid
	// with the probability that makes the expected payment the obligation (P10).
	if odds := payingOdds("dan/chain", "k3"); odds != 1 {
		t.Errorf("an obligation above the ticket was priced at %v, not certain", odds)
	}
	if odds := payingOdds("ana/echo", "k1"); odds <= 0 || odds >= 1 {
		t.Errorf("an obligation under the ticket was priced at %v, not a fraction", odds)
	}
}

// The catalogue's prices are declared once. The story publishes from that table and the shape
// prices the rail from it, so a price cannot be changed in one place and budgeted from another.
func TestEveryTradedActionHasADeclaredPrice(t *testing.T) {
	for _, tr := range crossKernelTrades {
		if _, ok := actionPrices[bareRef(tr.action)]; !ok {
			t.Errorf("%s is traded across a boundary but has no declared price", tr.action)
		}
	}
	for _, tr := range otherCrossKernelTrades {
		if _, ok := actionPrices[tr.action]; !ok {
			t.Errorf("%s is traded across a boundary but has no declared price", tr.action)
		}
	}
}

// Every cross-kernel trade must name a seller that is a real kernel and an action that is
// published on it, or the trading rounds quietly measure a catalogue of refusals.
func TestEveryCrossKernelTradeNamesARealSeller(t *testing.T) {
	handles := map[string]string{}
	for _, k := range kernelPlan {
		handles[k.handle] = k.name
	}
	for _, tr := range crossKernelTrades {
		at := strings.Index(tr.action, "@")
		slash := strings.Index(tr.action, "/")
		if at < 0 || slash < at {
			t.Errorf("%q is not a cross-kernel reference", tr.action)
			continue
		}
		handle := tr.action[at+1 : slash]
		if handles[handle] != tr.seller {
			t.Errorf("trade %q names seller %q, but the handle %q belongs to %q",
				tr.action, tr.seller, handle, handles[handle])
		}
	}
}

// px turns a price in credits into the base units the kernel counts, and every price in the story
// goes through it. A rail whose token has decimals would otherwise be asked to charge a millionth
// of the intended price.
func TestPricesScaleWithTheRail(t *testing.T) {
	for _, c := range []struct {
		scale, credits int64
		want           string
	}{{1, 10, "10"}, {1_000_000, 10, "10000000"}, {1_000_000, 0, "0"}} {
		s := &story{scale: c.scale}
		if got := s.px(c.credits); got != c.want {
			t.Errorf("scale %d, %d credits: got %s, want %s", c.scale, c.credits, got, c.want)
		}
	}
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// A public key is base64url and roughly one key in eight begins with a dash, which the command
// line then reads as an unknown flag. The failure is intermittent by construction — it depends on
// the key a kernel happens to generate — and it has already cost three separate commands in this
// suite, so it is checked rather than remembered.
func TestEveryCommandThatTakesAKeyPassesItAfterADoubleDash(t *testing.T) {
	for _, file := range []string{"story.go", "rail.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			// Only a command invocation can read a key as a flag.
			if !strings.Contains(line, ".Key") || !strings.Contains(line, ".Run(") {
				continue
			}
			if !strings.Contains(line, `"--"`) {
				t.Errorf(`%s:%d passes a public key positionally without a bare "--" first: %s`,
					file, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Every kernel in the story draws under the same face value and accepts the same maximum, so a
// payment costs the same wherever it is made and one kernel's rail bill is comparable with
// another's. A kernel that would not accept its own ticket refuses its configuration at boot, which
// a run discovers as a kernel that never came up with every later act broken.
func TestEveryKernelDrawsUnderTheSameFaceValue(t *testing.T) {
	s := &story{scale: 1}
	limits := []int64{sybilVictimHeadroom}
	for _, k := range kernelPlan {
		limits = append(limits, k.creditLimit)
	}
	for _, c := range limits {
		o := s.opts("k1", c)
		if o.Lottery != storyLottery {
			t.Errorf("limit %d: lottery %d, want %d", c, o.Lottery, storyLottery)
		}
		if o.LotteryMax < o.Lottery {
			t.Errorf("limit %d: accepts at most %d but draws for %d", c, o.LotteryMax, o.Lottery)
		}
		if o.CreditLimit != c {
			t.Errorf("limit %d: configured as %d", c, o.CreditLimit)
		}
	}
}
