package main

import (
	"os"
	"strings"
	"testing"
)

func TestRailsAreNamedAndNothingElseIs(t *testing.T) {
	for _, name := range []string{"play", "anvil", "sepolia"} {
		r, err := NewRail(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.Name() != name {
			t.Errorf("rail %q calls itself %q", name, r.Name())
		}
	}
	if _, err := NewRail("mainnet"); err == nil {
		t.Fatal("an unknown rail was accepted; a mistyped name must never fall through to a default")
	}
}

// Every world counts in millionths, so one number means the same amount on all of them. A price
// that did not scale would be a millionth of the one intended, which is not a refusal but a wrong
// economy that still runs.
func TestScaleFollowsTheWorldsMoney(t *testing.T) {
	if (playRail{}).Scale() != 1_000_000 {
		t.Error("play counts in millionths like the chains")
	}
	for _, name := range []string{"anvil", "sepolia"} {
		r, _ := NewRail(name)
		if r.Scale() != 1_000_000 {
			t.Errorf("%s: a six-decimal token needs a scale of 1,000,000, got %d", r.Name(), r.Scale())
		}
	}
}

// A rail may not choose how much work is done. Rounds cost no gas — a cross-kernel call
// accumulates a debt and the debt is paid once — so a rail that ran fewer of them would be running
// a smaller economy under the same name for no saving.
func TestNoRailCanChooseTheWorkload(t *testing.T) {
	src, err := os.ReadFile("rail.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "Rounds(") {
		t.Error("a rail decides how many trading rounds run; the workload is the story's, not the world's")
	}
}

// Each kernel must hold gas for every payment it will make. Funding it for one when it owes three
// leaves the second unsigned, the settlement at "pending", and the run reading like a stuck rail.
func TestGasIsProvisionedPerPaymentNotPerKernel(t *testing.T) {
	s := &sepoliaRail{}
	one := Shape{PayingUsers: 5, PaymentsPerKernel: map[string]int{"k1": 1, "k2": 1}}
	many := Shape{PayingUsers: 5, PaymentsPerKernel: map[string]int{"k1": 3, "k2": 1}}
	if s.cost(many) <= s.cost(one) {
		t.Error("a kernel that must sign three payments is not costed above one that signs once")
	}
}

// The paying wallet makes one token transfer per kernel it funds. Funding it for one transfer when
// it makes five leaves the later payments unsent, and those users silently unfunded.
func TestThePayingWalletIsCostedForEveryTransferItMakes(t *testing.T) {
	s := &sepoliaRail{}
	few := Shape{PayingUsers: 1, PaymentsPerKernel: map[string]int{"k1": 1}}
	lots := Shape{PayingUsers: 6, PaymentsPerKernel: map[string]int{"k1": 1}}
	if s.cost(lots) <= s.cost(few) {
		t.Error("more paying users did not raise the estimate")
	}
	// A mint is an ERC-20 write, not a plain send. Pricing it as a send understates every deposit.
	if sepMintFee <= sepTxFee {
		t.Error("a token mint is priced at or below a plain ETH send")
	}
}

// The point of pricing the story in advance: a rail that cannot afford it says so with numbers and
// refuses, rather than running a cheaper economy under the same name.
//
// The canonical story does not fit the default cap, and is not meant to: every cross-kernel call
// that owes draws its own ticket and a winning draw is its own payment, so twelve rounds and a
// burst come to roughly a hundred and thirty payments. That is a measured fact about this economy
// on this chain, not a defect in the suite, and the suite's job is to state it and stop — the
// operator raises JUICE_SEPOLIA_BUDGET deliberately or runs it on anvil. The figures are pinned so
// that a change to the story or the fee model shows up as a failing test rather than as a silently
// different bill.
func TestTheCanonicalStoryIsPricedBeforeAnythingIsSpent(t *testing.T) {
	s := &sepoliaRail{}
	shape := StoryShape(12)
	cost := s.cost(shape)
	if cost < 0.018 || cost > 0.026 {
		t.Errorf("the canonical story now costs %.6f ETH, outside the expected band. If the trade "+
			"graph changed on purpose, re-pin this figure and the note in docs/network-simulation.md", cost)
	}
	// Each additional payment a kernel must sign has to raise the estimate, or the estimate is not
	// measuring the thing that actually costs money.
	bigger := shape
	bigger.PaymentsPerKernel = map[string]int{}
	for k, n := range shape.PaymentsPerKernel {
		bigger.PaymentsPerKernel[k] = n + 1
	}
	if s.cost(bigger) <= cost {
		t.Error("more payments per kernel did not raise the estimate")
	}
	// A kernel funded for one payment when it owes three stalls on the second. Pricing by the
	// number of kernels rather than the number of payments is what produces that.
	one := shape
	one.PaymentsPerKernel = map[string]int{"k1": 1, "k2": 1, "k3": 1, "k4": 1}
	if s.cost(one) >= cost {
		t.Error("the estimate does not distinguish a kernel that signs once from one that signs three times")
	}
}

// The chain allowlist is the one check that stands between a mistyped RPC and real money. It names
// what is permitted, never what is forbidden.
func TestTheChainAllowlistNamesWhatIsPermitted(t *testing.T) {
	src, err := os.ReadFile("rail.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, `chainID != "421614"`) {
		t.Error("the sepolia rail must refuse every chain but Arbitrum Sepolia (421614) by name")
	}
	for _, mainnet := range []string{`"1"`, `"42161"`, `"8453"`, `"10"`} {
		if strings.Contains(body, "chainID == "+mainnet) {
			t.Errorf("the rail compares against %s; a denylist leaves every other real network one "+
				"mistyped RPC away", mainnet)
		}
	}
}

// A funding key belongs in the file it came from. Writing one into the run directory gives it a
// second home to leak from, and the run directory is an artifact people share.
func TestNoRailWritesAKeyIntoTheRunDirectory(t *testing.T) {
	src, _ := os.ReadFile("rail.go")
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "os.WriteFile") &&
			(strings.Contains(line, "key") || strings.Contains(line, "spender")) {
			t.Errorf("a rail writes a key to disk: %s", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(string(src), "n.Secret(fkey") {
		t.Error("the funding key must be registered for redaction the moment it is read")
	}
}

// A public key is base64url and may begin with a dash, so wherever the simulation hands one to a
// command as a positional argument it must come after a bare `--`. Without it the key is read as an
// unknown flag and the command fails in a way the simulation reports as the kernel's fault.
func TestEveryKeyIsPassedAfterADoubleDash(t *testing.T) {
	for _, name := range []string{"rail.go", "story.go", "harness.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			open := strings.Index(line, "(")
			if open < 0 {
				continue
			}
			// Only a key handed to a command counts; one being read back into a field does not.
			args := line[open:]
			if !strings.Contains(args, ".Key") || !strings.Contains(args, `"admin"`) {
				continue
			}
			if !strings.Contains(args, `"--"`) {
				t.Errorf("%s:%d passes a public key with no bare -- before it: %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// A run reports what it allocated beside what it estimated. An estimate nobody compares with the
// bill stays wrong until a run stalls: counting trading partners instead of payments survived a
// release that way, funding the busiest buyer for two payments where it made thirty-three.
func TestTheRunReportsWhatItSpentAgainstWhatItEstimated(t *testing.T) {
	src, err := os.ReadFile("rail.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"estimated_eth", "allocated_eth", "s.estimate"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the Sepolia rail does not report %s, so a wrong estimate is invisible", want)
		}
	}
}
