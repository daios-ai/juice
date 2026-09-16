// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The story is one economy, and it is the same economy on every rail.
//
// Every participant, action, price, trade, composition, refusal, attack and assertion below is
// fixed here and is executed unchanged on play, anvil and Sepolia. The rail supplies only how
// money enters, how a payment is made and becomes final, and what the run cost. That is what makes
// the three reports comparable: a difference between them is a difference in the rail, because
// nothing else differed.
//
// One quantity varies: how many times the trading rounds repeat, which the rail chooses because a
// live testnet charges for each round in gas and in a quarter of an hour of finality. Every
// distinct event still happens at least once everywhere, and the report states the count.

// participants. Five kernels with deliberately unlike economics, so a trade crossing any pair is
// priced differently and a mistake in the pricing rule cannot cancel itself out.
var kernelPlan = []struct {
	name                         string
	handle                       string
	feeBps, remoteBps, importBps int
	creditLimit                  int64
}{
	// The credit limit must outlast the trading rounds. It bounds what a kernel has delivered and
	// not been paid for, and here that is one round's obligations at most — every one of them is
	// settled before the round ends, because a seller serves nobody who still owes it. The low limit
	// that makes the engine refuse is exercised in the Sybil act instead.
	{"k1", "hub", 2000, 500, 500, 30000},
	{"k2", "shop", 1000, 700, 300, 30000},
	{"k3", "maker", 2500, 0, 1000, 30000},
	{"k4", "buyer", 0, 500, 0, 30000},
	{"k5", "late", 1500, 1000, 500, 30000},
}

// Users, and how money reaches them.
//
// It enters each kernel at one point and spreads from there by ordinary fee-free transfers between
// local users. That is what an economy does, and it is also what a chain requires: a payout address
// belongs to one account on a kernel, so two users on the same kernel cannot both pay in from the
// same wallet — the first to register absorbs the second's money. Funding everyone independently
// would need a wallet each, which on a rationed testnet is the difference between a gate that can
// be run and one that cannot. The shape is the same on every rail.
var userPlan = []struct {
	handle   string
	on       string
	bringsIn int64 // paid in from outside the economy
	receives int64 // transferred from whoever brought money into this kernel
}{
	{"ana", "k1", 18000, 0}, {"ben", "k1", 0, 6000},
	{"cara", "k2", 9000, 0},
	{"dan", "k3", 16000, 0}, {"eve", "k3", 0, 4000},
	{"fay", "k4", 16000, 0}, {"gus", "k4", 0, 4000},
	{"hal", "k5", 6000, 0},
}

// actionPrices is what every action in the catalogue advertises, in credits. The story publishes
// from this and the shape prices the rail from it, so what a buyer is charged and what the run is
// funded for cannot drift apart.
var actionPrices = map[string]int64{
	"ana/echo": 10, "ana/helper": 5, "ana/local-only": 7, "ben/index": 3,
	"cara/quote": 25, "cara/premium": 200, "cara/flaky": 33, "cara/badout": 11,
	"dan/bundle": 60, "gus/index": 0, "hal/service": 40,
	"dan/chain": 120, "cara/pair": 90, // the composites
}

// The paid trades that cross a kernel boundary. Each opens a debt from the buyer's kernel to the
// seller's, and each of those debts is settled. This is the trade graph, and it is the same
// everywhere.
var crossKernelTrades = []struct{ kernel, user, action, seller string }{
	{"k3", "dan", "cara@shop/quote", "k2"},
	{"k1", "ana", "cara@shop/quote", "k2"},
	{"k4", "fay", "cara@shop/quote", "k2"},
	{"k2", "cara", "ana@hub/echo", "k1"},
	{"k3", "eve", "ana@hub/echo", "k1"},
	{"k4", "fay", "ana@hub/echo", "k1"},
	{"k1", "ben", "dan@maker/bundle", "k3"},
	{"k4", "gus", "dan@maker/bundle", "k3"},
	{"k2", "cara", "dan@maker/bundle", "k3"},
	{"k1", "ben", "dan@maker/chain", "k3"}, // a composite that itself buys across a boundary
}

// The calls the other acts make across a kernel boundary, with how many each act makes. They are
// declared here, beside the trading rounds, because a payment they cause costs exactly as much as
// one the rounds cause and a rail that was not told about them runs out of gas partway through.
// `calls` is what the act does at most: the burst fires a fixed number, the Sybil act loops a fixed
// number, and the late joiner polls until it is discovered.
var otherCrossKernelTrades = []struct {
	kernel, action, seller string
	calls                  int
}{
	{"k4", "cara/quote", "k2", 80},  // the burst that closes the trading rounds
	{"k1", "hal/service", "k5", 15}, // the late joiner sells to an established kernel
	{"k5", "cara/quote", "k2", 1},   // and buys from one
	{"k6", "cara/quote", "k2", 12},  // the two Sybil identities borrow from the shop
	{"k7", "cara/quote", "k2", 12},
	{"k3", "cara/quote", "k2", 12}, // an honest kernel borrows alongside them
}

// payingOdds is how likely one cross-kernel call is to cost a payment. The call owes
// `D = mp + the seller's markup` and the ticket pays the whole face value with probability D/L,
// or exactly D when D is at or above it (P10) — which is what turns hundreds of calls into a
// handful of payments, and what a chain rail's bill actually measures.
func payingOdds(action, seller string) float64 {
	mp := actionPrices[action]
	if mp <= 0 {
		return 0 // a free call owes nothing and settles no payment
	}
	rates := storyRates()[seller]
	owed := mp + int64(math.Ceil(float64(mp*int64(rates.remoteBps))/10000))
	if owed >= storyLottery {
		return 1
	}
	return float64(owed) / float64(storyLottery)
}

// StoryShape is what the story will ask of the rail, declared before anything runs so a rail with
// a budget can price it and refuse in advance rather than run dry halfway through. It depends on
// the number of trading rounds, because every cross-kernel call that owes draws its own ticket and
// every winning draw is its own payment (P10): counting the ordered pairs a story forms, as this
// once did, understates a chain's bill by the number of rounds.
func StoryShape(rounds int) Shape {
	debtors := map[string]bool{}
	pairs := map[string]bool{}
	expected := map[string]float64{}
	for _, t := range crossKernelTrades {
		debtors[t.kernel], pairs[t.kernel+"->"+t.seller] = true, true
		expected[t.kernel] += float64(rounds) * payingOdds(bareRef(t.action), t.seller)
	}
	for _, t := range otherCrossKernelTrades {
		debtors[t.kernel], pairs[t.kernel+"->"+t.seller] = true, true
		expected[t.kernel] += float64(t.calls) * payingOdds(t.action, t.seller)
	}
	// A draw is a coin, so the count is a mean and a run may be unlucky. Half again plus one covers
	// three standard deviations at these numbers, and a vault funded short stalls the whole story.
	perKernel := map[string]int{}
	for k, e := range expected {
		perKernel[k] = int(math.Ceil(e*1.5)) + 1
	}
	// Only the users who bring money in from outside cost a chain rail anything; the rest are
	// funded by a transfer inside the kernel, which the chain never sees.
	payers := 0
	for _, u := range userPlan {
		if u.bringsIn > 0 {
			payers++
		}
	}
	// The attackers pay their own way, so each is a paying user too.
	payers += len(sybilPlan)
	return Shape{
		Kernels:           len(kernelPlan),
		PayingUsers:       payers,
		SigningKernels:    len(debtors),
		Settlements:       len(pairs),
		PaymentsPerKernel: perKernel,
	}
}

// bareRef drops the kernel from a reference: `cara@shop/quote` is `cara/quote` in the catalogue.
func bareRef(ref string) string {
	at, slash := strings.Index(ref, "@"), strings.Index(ref, "/")
	if at < 0 || at > slash {
		return ref
	}
	return ref[:at] + ref[slash:]
}

// shortOfMoney is how the kernel says a caller cannot afford a call. It names the balance and the
// price rather than using the word "insufficient", which is the better message and the one to
// match.
const shortOfMoney = `credits, call costs|insufficient|balance|funds`

// sybilPlan is the attacker's identities. They are named here so the declared shape counts their
// deposits and their debts: an attacker who cannot pay for a call is refused for want of money and
// never reaches the credit limit, which is the thing under test.
var sybilPlan = []string{"k6", "k7"}

// The attackers are kernels too, and the oracle must be able to predict what they were charged.
const (
	sybilFeeBps    = 1000
	sybilRemoteBps = 500
	sybilImportBps = 500
)

// sybilVictimHeadroom is how much unpaid delivered work the shop will carry beyond wherever its
// honest trading has already left it, for the length of the attack. It is a couple of the shop's own
// calls rather than a round number, because the counter moves in both directions while the attack
// runs — cash from earlier trades keeps landing, and on a slow rail it can land faster than three
// attackers can borrow. A headroom the attack crosses in its first few calls is reached whatever the
// drift, which is what makes the refusal it is looking for actually happen.
const sybilVictimHeadroom = 60

// StoryVersion changes whenever the economy does, so two reports are never compared as if they
// measured the same thing.
const StoryVersion = "3"

const (
	schemaIn  = `{"type":"object","properties":{"msg":{"type":"string","description":"text to send"}}}`
	schemaOut = `{"type":"object","properties":{"echo":{"type":"string","description":"the echoed text"}},"required":["echo"]}`
)

type story struct {
	n      *Net
	scale  int64
	boot   string // the multiaddress newcomers bootstrap from
	shape  Shape
	opened []settlement // every settlement opened, for the report's evidence

	recoverySeconds  float64 // restart of a killed provider to the first trade that worked again
	burstCallsPerSec float64 // cross-kernel calls a second under a fixed concurrent load

	// What the story put into the economy from outside, and what it advertised each action for.
	// The oracle predicts from these; reading them back off the kernel would only prove the kernel
	// agrees with itself.
	awaiting []funding // payments submitted and not yet counted
	deposits Deposits
	prices   map[string]int64  // bare action reference -> advertised price in credits
	owners   map[string]string // bare action reference -> the kernel that serves it
}

// funding is a payment submitted from outside the economy, and what it should come to.
type funding struct {
	kernel, user string
	want         int64
}

// deposited records money entering the economy from outside, which is the only thing that changes
// the total the network holds.
func (s *story) deposited(kernel string, credits int64) {
	if s.deposits.By == nil {
		s.deposits.By = map[string]int64{}
	}
	s.deposits.By[kernel] += credits * s.scale
	s.deposits.Total += credits * s.scale
}

// settlement is one obligation the story saw closed: what the buyer owed, what actually moved on the
// rail for it, and whether the seller's books closed against it. The report checks that the money
// that moved is what the draw decided, and that the seller was credited what it was owed.
type settlement struct {
	ID         string `json:"id"`
	Debtor     string `json:"debtor"`
	Creditor   string `json:"creditor"`
	Amount     int64  `json:"amount"`     // what moved on the rail, once the buyer has said
	Obligation int64  `json:"obligation"` // what the buyer owed
	Status     string `json:"status"`     // empty until the buyer says how its draw came out
	Closed     bool   `json:"closed"`
	WallMs     int64  `json:"wall_ms"`
}

// px converts an amount in credits to the base units the kernel counts in, for every cap and
// balance comparison the story makes against a number the kernel reported, and for an argument
// carried inside a JSON payload. Command-line money arguments are the exception and take credits
// as a person writes them: the CLI scales those itself, in the world's own unit (D20).
func (s *story) px(credits int64) string { return strconv.FormatInt(credits*s.scale, 10) }

// cr is a price as a command line takes it: the number a person writes.
func cr(credits int64) string { return strconv.FormatInt(credits, 10) }

func Run(n *Net, rounds int) (*story, error) {
	s := &story{n: n, scale: n.Rail.Scale(), shape: StoryShape(rounds),
		prices: map[string]int64{}, owners: map[string]string{}}

	for _, act := range []struct {
		name string
		fn   func() error
	}{
		{"the kernels come up", s.actKernels},
		{"money enters", s.actMoney},
		{"the catalogue", s.actCatalogue},
		{"trading", func() error { return s.actTrading(rounds) }},
		{"refusals", s.actRefusals},
		{"composition and partial refunds", s.actComposition},
		{"steps", s.actSteps},
		{"value transfers", s.actValue},
		{"delegated authorization", s.actDelegated},
		{"ratings and evidence", s.actEvidence},
		{"a fifth kernel joins", s.actLateJoiner},
		{"a provider is killed mid-economy", s.actChurn},
		{"attacks", s.actAttacks},
		{"everything outstanding is settled", s.actSettleAll},
	} {
		n.Scenario(act.name)
		fmt.Printf("== %s\n", act.name)
		if err := act.fn(); err != nil {
			return s, fmt.Errorf("%s: %w", act.name, err)
		}
	}
	return s, nil
}

func (s *story) k(name string) *Kernel { return s.n.Kernels[name] }

// storyLottery is the ticket face value every kernel in the story draws for, in credits. It sits
// above most obligations here and below some, so both branches of the draw are exercised: an
// obligation under it is paid with probability D/L, and one at or above it is paid exactly. It is
// what turns hundreds of calls into a handful of payments, which is the property the rail cost of
// this run measures.
const storyLottery = 100

// opts is how every kernel in the story is configured: from its plan entry, with the credit limit
// as given. Every kernel draws under the same face value, so a payment costs the same wherever it
// is made and the report can compare one kernel's rail bill with another's.
func (s *story) opts(name string, limit int64) bootOpts {
	fee, remote, imp, handle := sybilFeeBps, sybilRemoteBps, sybilImportBps, name
	for _, k := range kernelPlan {
		if k.name == name {
			fee, remote, imp, handle = k.feeBps, k.remoteBps, k.importBps, k.handle
		}
	}
	return bootOpts{Handle: handle, FeeBps: fee, RemoteBps: remote, ImportBps: imp,
		CreditLimit: limit * s.scale, Lottery: storyLottery * s.scale,
		LotteryMax:   storyLottery * s.scale,
		RetrySeconds: 2, Bootstrap: s.boot}
}

// join boots a kernel into the economy: gas for the payments it will make, and names exchanged
// with every kernel already up. Petnames are the local operator's own labels and are never taken
// from the network, so each side is told what to call the other — which is also what the squatting
// attack tests later.
func (s *story) join(name string, cap int64, handle string) (*Kernel, error) {
	o := s.opts(name, cap)
	if handle != "" {
		o.Handle = handle
	}
	k, err := s.n.Boot(name, o)
	if err != nil {
		return nil, err
	}
	if s.boot == "" {
		s.boot = k.FedAddr()
	}
	if err := s.n.Rail.GasUp(k, s.shape.PaymentsPerKernel[name]); err != nil {
		return nil, err
	}
	for other, ok := range s.n.Kernels {
		if other == name || ok.URL == "" {
			continue
		}
		_, _ = ok.Run("sysop-"+other, "admin", "peer", "rename", "--", k.Key, handleOf(name))
		_, _ = k.Run("sysop-"+name, "admin", "peer", "rename", "--", ok.Key, handleOf(other))
	}
	return k, nil
}

// fund submits money into the economy from outside and records that it did: deposits are the only
// thing that changes the total the network holds, and the oracle predicts from this record. On a
// chain the credit appears only once the payment is final, so the waiting is awaitFunds's.
func (s *story) fund(kernel, user string, credits int64) error {
	if err := s.n.Rail.Fund(s.k(kernel), user, credits); err != nil {
		return err
	}
	s.deposited(kernel, credits)
	s.awaiting = append(s.awaiting, funding{kernel, user, credits * s.scale})
	return nil
}

// awaitFunds waits for every payment submitted since the last call to be counted. Waiting for each
// in turn would cost a public chain's finality apiece — a quarter of an hour for each of seven
// payers — where waiting for all of them together costs one.
func (s *story) awaitFunds() error {
	pending := s.awaiting
	s.awaiting = nil
	if len(pending) == 0 {
		return nil
	}
	if !poll(30*time.Minute, 5*time.Second, func() bool {
		for _, f := range pending {
			if s.k(f.kernel).Balance(f.user) < f.want {
				return false
			}
		}
		return true
	}) {
		var short []string
		for _, f := range pending {
			if got := s.k(f.kernel).Balance(f.user); got < f.want {
				short = append(short, fmt.Sprintf("%s on %s holds %d of %d", f.user, f.kernel, got, f.want))
			}
		}
		return fmt.Errorf("payments were never credited: %s", strings.Join(short, "; "))
	}
	return nil
}

// publish puts an HTTP action in the catalogue and records its price and home for the oracle.
func (s *story) publish(kernel, owner, name, visibility, route, desc string) {
	k, credits := s.k(kernel), actionPrices[owner+"/"+name]
	s.prices[owner+"/"+name] = credits
	s.owners[owner+"/"+name] = kernel
	_, _ = k.Run(owner, "action", "create", name, "--kind", "http", "--source", s.n.Backend+route,
		"--description", desc, "--price", cr(credits), "--input-schema", schemaIn, "--output-schema", schemaOut)
	_, _ = k.Run(owner, "action", "enable", owner+"/"+name)
	if visibility != "private" {
		_, _ = k.Run(owner, "action", "update", owner+"/"+name, "--visibility", visibility)
	}
}

// handleOf is the network name a kernel advertises.
func handleOf(name string) string {
	for _, k := range kernelPlan {
		if k.name == name {
			return k.handle
		}
	}
	return name
}

// rates is what each kernel charges, which the oracle needs to predict a cross-kernel price.
func storyRates() map[string]kernelRates {
	out := map[string]kernelRates{}
	for _, k := range kernelPlan {
		out[k.name] = kernelRates{remoteBps: k.remoteBps, importBps: k.importBps}
		out[k.handle] = kernelRates{remoteBps: k.remoteBps, importBps: k.importBps}
	}
	// The attackers buy too, and what they were charged is as much a price to check as anyone's.
	for _, name := range append(append([]string{}, sybilPlan...), "k8") {
		out[name] = kernelRates{remoteBps: sybilRemoteBps, importBps: sybilImportBps}
	}
	return out
}

// ---- the kernels come up ----------------------------------------------------

func (s *story) actKernels() error {
	for _, p := range kernelPlan[:4] { // the fifth joins later, into an economy already running
		if _, err := s.join(p.name, p.creditLimit, ""); err != nil {
			return err
		}
	}
	return nil
}

// ---- money enters -----------------------------------------------------------

func (s *story) actMoney() error {
	for _, u := range userPlan {
		if u.on == "k5" {
			continue // the fifth kernel joins later
		}
		s.k(u.on).MakeUser(u.handle)
	}
	// One payment into each kernel, from outside.
	payer := map[string]string{}
	for _, u := range userPlan {
		if u.on == "k5" || u.bringsIn == 0 {
			continue
		}
		payer[u.on] = u.handle
		if err := s.fund(u.on, u.handle, u.bringsIn); err != nil {
			return err
		}
	}
	if err := s.awaitFunds(); err != nil {
		return err
	}
	// Then it spreads, as it would. A transfer between local users carries no fee and needs no
	// rail: the money is already inside the kernel.
	//
	// Note the units. `user transfer`, `user withdraw` and `admin user deposit` take an amount as a
	// person writes it and scale it by the world's decimals themselves, while every price and
	// balance elsewhere in this story is in base units. Passing base units here funds nobody, and
	// does so silently.
	for _, u := range userPlan {
		if u.on == "k5" || u.receives == 0 {
			continue
		}
		src := payer[u.on]
		if src == "" {
			return fmt.Errorf("%s on %s is to receive money but nobody paid into that kernel", u.handle, u.on)
		}
		s.n.MustWork("money.spreads_by_local_transfer", s.k(u.on), src,
			"user", "transfer", "--yes", u.handle, strconv.FormatInt(u.receives, 10))
		if got := s.k(u.on).Balance(u.handle); got < u.receives*s.scale {
			return fmt.Errorf("%s on %s holds %d after a transfer of %d", u.handle, u.on, got, u.receives*s.scale)
		}
	}
	// Money enters only against a named payment, and the same payment never moves money twice.
	// Submitting one three times must credit it once, on every rail.
	k1 := s.k("k1")
	before := k1.Balance("ana")
	_, _ = k1.Run("sysop-k1", "admin", "user", "deposit", "--yes", "ana", "100", "--ref", "netsim-replay")
	afterFirst := k1.Balance("ana")
	s.deposited("k1", (afterFirst-before)/s.scale)
	for i := 0; i < 2; i++ {
		_, _ = k1.Run("sysop-k1", "admin", "user", "deposit", "--yes", "ana", "100", "--ref", "netsim-replay")
	}
	// Whether the first submission credits anything is the rail's business: where the operator's
	// record is the fact it credits, and where a chain is the fact it is refused until the chain
	// has seen the payment. What no rail may do is credit the same reference twice.
	s.n.Check("money.one_payment_credited_once", k1.Balance("ana") == afterFirst,
		fmt.Sprintf("resubmitting one payment moved a further %d (%d after the first submission, "+
			"%d after two more)", k1.Balance("ana")-afterFirst, afterFirst-before, k1.Balance("ana")-before))
	return nil
}

// ---- the catalogue ----------------------------------------------------------

func (s *story) actCatalogue() error {
	s.publish("k1", "ana", "echo", "public", "/echo", "Echo a message back to the caller")
	s.publish("k1", "ana", "helper", "private", "/echo", "A private helper, reachable only by its owner")
	s.publish("k1", "ana", "local-only", "local", "/echo", "Echo restricted to this kernel's own users")
	s.publish("k1", "ben", "index", "public", "/echo", "Ben's front door")
	s.publish("k2", "cara", "quote", "public", "/echo", "Return a price quote")
	s.publish("k2", "cara", "premium", "public", "/echo", "Premium analysis")
	s.publish("k2", "cara", "flaky", "public", "/flaky", "A service whose upstream fails intermittently")
	s.publish("k2", "cara", "badout", "public", "/badout", "A service that returns the wrong shape")
	s.publish("k3", "dan", "bundle", "public", "/echo", "A service others buy, priced above its cost")
	s.publish("k4", "gus", "index", "public", "/echo", "A free front door")

	// The composites. dan/chain buys a service on another kernel, so its price must cover the
	// imported quote — the remote price plus both operators' cuts. cara/pair buys two of cara's
	// own actions in order: the first settles, the second returns the wrong shape and fails, which
	// is the only way to observe a partial refund.
	chain := filepath.Join(s.n.Root, "chain.wasm")
	if err := writeComposite(chain, "ana@hub/echo"); err != nil {
		return err
	}
	pair := filepath.Join(s.n.Root, "pair.wasm")
	if err := writeComposite(pair, "cara/quote", "cara/badout"); err != nil {
		return err
	}
	s.n.MustWork("catalogue.composite_across_kernels", s.k("k3"), "dan", "action", "create", "chain",
		"--kind", "wasm", "--source", chain, "--price", cr(actionPrices["dan/chain"]),
		"--description", "A composite that buys a service on another kernel")
	_, _ = s.k("k3").Run("dan", "action", "enable", "dan/chain")
	// A composite nobody but its owner may call is a composite that never composes: the cross-
	// kernel trade below buys this one, and so does another user on its own kernel.
	_, _ = s.k("k3").Run("dan", "action", "update", "dan/chain", "--visibility", "public")
	s.n.MustWork("catalogue.composite_partial", s.k("k2"), "cara", "action", "create", "pair",
		"--kind", "wasm", "--source", pair, "--price", cr(actionPrices["cara/pair"]),
		"--description", "Buys a quote, then a service that returns the wrong shape")
	_, _ = s.k("k2").Run("cara", "action", "enable", "cara/pair")
	_, _ = s.k("k2").Run("cara", "action", "update", "cara/pair", "--visibility", "public")
	return nil
}

// ---- trading ----------------------------------------------------------------

// buy makes one purchase. A refusal because the seller is still owed for work it already delivered
// is not a failure: it is the credit engine doing its job, and the answer is to settle and try
// again. That is also what the economy looks like in practice — a seller serves the buyers who have
// paid it.
func (s *story) buy(kernel, user, action string) (ok bool, needsSettlement bool) {
	out, err := s.k(kernel).Run(user, "--json", "run", action, `{"msg":"netsim"}`)
	if err == nil && strings.Contains(out, "tx_id") {
		return true, false
	}
	return false, strings.Contains(out, "credit with peer") || strings.Contains(out, "exhausted")
}

func (s *story) actTrading(rounds int) error {
	local := []struct{ kernel, user, action string }{
		{"k1", "ben", "ana/echo"}, {"k1", "ana", "ben/index"},
		{"k2", "cara", "cara/quote"}, {"k3", "dan", "dan/bundle"},
		{"k4", "gus", "gus/index"}, {"k1", "ana", "ana/local-only"},
		{"k2", "cara", "cara/flaky"}, {"k2", "cara", "cara/badout"},
	}
	ok, refused, settled := 0, 0, 0
	for r := 1; r <= rounds; r++ {
		for _, t := range local {
			if good, _ := s.buy(t.kernel, t.user, t.action); good {
				ok++
			} else {
				refused++
			}
		}
		for _, t := range crossKernelTrades {
			good, needs := s.buy(t.kernel, t.user, t.action)
			if needs {
				if s.settle(t.kernel, t.seller) == nil {
					settled++
				}
				good, _ = s.buy(t.kernel, t.user, t.action)
			}
			if good {
				ok++
			} else {
				refused++
			}
		}
		if r%5 == 0 || r == rounds {
			fmt.Printf("  round %d/%d: %d bought, %d refused, %d settlements\n", r, rounds, ok, refused, settled)
		}
	}
	fmt.Printf("  trading done: %d bought, %d refused, %d settlements\n", ok, refused, settled)
	s.burst()
	return nil
}

// burst is a fixed concurrent load, so the report can say what the network sustains rather than
// only how long one call takes when nothing else is happening. The calls cross a kernel boundary:
// a burst of local calls would measure one process talking to itself.
func (s *story) burst() {
	const callers, each = 8, 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	start := time.Now()
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				out, err := s.k("k4").Run("fay", "--json", "run", "cara@shop/quote", `{"msg":"burst"}`)
				if err == nil && strings.Contains(out, "tx_id") {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed > 0 {
		s.burstCallsPerSec = float64(ok) / elapsed
	}
	fmt.Printf("  burst: %d of %d cross-kernel calls in %.1fs (%.1f/s)\n",
		ok, callers*each, elapsed, s.burstCallsPerSec)
}

// owed is how many obligations a buyer still owes a seller: what the seller has delivered and not
// been paid for. Zero means nothing is outstanding between them — which is why a read that failed
// must say so rather than answer zero: nothing is now the whole evidence that a debt was paid, and
// a kernel that cannot be read has told us nothing at all.
func (s *story) owed(seller, buyer string) (int, error) {
	rows, err := s.outstanding(seller)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, st := range rows {
		if s.isDebtor(buyer, st.Debtor) {
			n++
		}
	}
	return n, nil
}

// isDebtor reports whether an obligation's named debtor is this kernel, however the seller renders
// it — by the petname it bound or by the raw key.
func (s *story) isDebtor(buyer, debtor string) bool {
	k := s.k(buyer)
	return k != nil && (debtor == k.Handle || debtor == k.Key || strings.HasSuffix(debtor, k.Key[:8]))
}

// outstanding is what a seller is waiting to be paid for, as the operator sees it: every obligation
// still open, named by the fact that will close it.
func (s *story) outstanding(seller string) ([]settlement, error) {
	out, err := s.k(seller).Run("sysop-"+seller, "--json", "admin", "kernel", "deposits")
	if err != nil {
		return nil, fmt.Errorf("reading what %s is owed: %w", seller, err)
	}
	var body struct {
		Owed []struct {
			ID         string `json:"id"`
			Peer       string `json:"peer"`
			Obligation int64  `json:"obligation"`
			Amount     int64  `json:"amount"`
			Status     string `json:"status"`
		} `json:"owed"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		return nil, fmt.Errorf("reading what %s is owed: %w", seller, err)
	}
	found := make([]settlement, 0, len(body.Owed))
	for _, r := range body.Owed {
		found = append(found, settlement{ID: r.ID, Creditor: seller, Debtor: r.Peer,
			Amount: r.Amount, Obligation: r.Obligation, Status: r.Status})
	}
	return found, nil
}

// settle waits for what a buyer owes a seller to be paid. Nobody is asked: the buyer commits its
// payment when it settles the call, its rail sends it, and the seller's books close against the
// finalized fact — the chain's own record where there is a chain, the buyer's signed reveal where
// there is none. So this watches, it does not act, and it records each obligation it saw so the
// report can say what was owed and what actually moved.
func (s *story) settle(buyer, seller string) error {
	if s.k(buyer) == nil || s.k(seller) == nil {
		return fmt.Errorf("no such kernel")
	}
	seen := map[string]settlement{}
	record := func(closed bool) {
		for _, st := range seen {
			st.Closed = closed
			s.opened = append(s.opened, st)
		}
	}
	deadline := time.Now().Add(s.n.Rail.SettleWait())
	for {
		// One reading answers both questions, so what is counted is what was looked at.
		rows, err := s.outstanding(seller)
		if err != nil {
			record(false)
			return err
		}
		open := 0
		for _, st := range rows {
			if st.Debtor == "" || !s.isDebtor(buyer, st.Debtor) {
				continue
			}
			open++
			// The same obligation is seen on every pass, and what the draw decided is known only
			// once its buyer has said: keep the latest reading, and never lose one that did.
			if was, ok := seen[st.ID]; !ok || st.Status != "" || was.Status == "" {
				seen[st.ID] = st
			}
		}
		if open == 0 {
			record(true)
			return nil
		}
		if time.Now().After(deadline) {
			record(false)
			return fmt.Errorf("%s still owes %s for %d calls", buyer, seller, open)
		}
		time.Sleep(time.Second)
	}
}

// settleAll waits for every obligation in a set of ordered pairs to be paid. A pair whose books
// cannot be read is not a pair with nothing outstanding: it is counted as failed, so a kernel that
// has stopped answering can never read as an economy that settled.
func (s *story) settleAll(pairs [][2]string) (done, failed int) {
	for _, p := range pairs {
		if s.k(p[0]) == nil || s.k(p[1]) == nil {
			continue
		}
		n, err := s.owed(p[1], p[0])
		if err != nil {
			failed++
			continue
		}
		if n == 0 {
			continue
		}
		if s.settle(p[0], p[1]) == nil {
			done++
		} else {
			failed++
		}
	}
	return done, failed
}

// ---- refusals ---------------------------------------------------------------

func (s *story) actRefusals() error {
	n, k1, k4 := s.n, s.k("k1"), s.k("k4")
	n.MustRefuse("refuse.private_action", "not found|denied|permitted|private", k1, "ben", "run", "ana/helper", `{"msg":"x"}`)
	n.MustRefuse("refuse.wrong_input_type", "schema|string|invalid|expected", k1, "ben", "run", "ana/echo", `{"msg":12345}`)
	n.MustRefuse("refuse.unknown_action", "not found|no such|unknown", k1, "ben", "run", "ana/nosuch", `{}`)
	// gus is funded, so the caller who cannot afford this must be one who genuinely cannot: a new
	// account with nothing. Asserting a refusal that the balance does not actually force measures
	// nothing.
	k4.MakeUser("skint")
	n.MustRefuse("refuse.beyond_balance", shortOfMoney, k4, "skint", "run", "cara@shop/premium", `{}`)

	// A refusal must cost nothing. G6 puts the refusal before anything is locked, so the balance
	// after a rejected call is the balance before it.
	before := k1.Balance("ben")
	_, _ = k1.Run("ben", "run", "ana/nosuch", `{}`)
	n.Check("refuse.nothing_locked", k1.Balance("ben") == before,
		fmt.Sprintf("a refused call moved the balance from %d to %d", before, k1.Balance("ben")))
	return nil
}

// ---- composition and partial refunds ---------------------------------------

func (s *story) actComposition() error {
	// A composite that succeeds is charged its advertised price exactly, whatever it spent inside:
	// the price the caller agreed to is the price, and a subtree cannot raise it.
	// The buyer must not be the owner. An owner calling their own action pays the operator's fee
	// and receives the rest back as its provider, so measuring the price on them would report a
	// quarter of it and call the rule broken.
	k3, k2 := s.k("k3"), s.k("k2")
	before := k3.Balance("eve")
	_, _ = k3.Run("eve", "--json", "run", "dan/bundle", `{"msg":"priced"}`)
	charged := before - k3.Balance("eve")
	s.n.Check("compose.charged_the_advertised_price", charged == 60*s.scale,
		fmt.Sprintf("a call priced %d charged %d", 60*s.scale, charged))

	// The partial refund. cara/pair buys a quote that settles and then a service that fails, so
	// the composite fails with one descendant already paid. The caller must be refunded the price
	// less exactly what that descendant consumed. The arithmetic is checked over every transaction
	// tree in the report; running it here is what makes the case exist.
	for i := 0; i < 3; i++ {
		_, _ = k2.Run("cara", "--json", "run", "cara/pair", `{"msg":"partial"}`)
	}
	s.n.MustWork("compose.tree_readable", k2, "cara", "tx", "list", "--limit", "20")

	// The composite is a generated WebAssembly module, registered and executed through the same
	// binary an operator would use. Showing that it ran is not the same as showing it compiled: a
	// module can be accepted at registration and fail to instantiate at the first call, which is
	// exactly what a wrong constant offset did here.
	var out string
	ran := poll(30*time.Second, 3*time.Second, func() bool {
		out, _ = k3.Run("eve", "--json", "run", "dan/chain", `{"msg":"composite"}`)
		return strings.Contains(out, "tx_id")
	})
	s.n.Check("compose.wasm_executes_through_the_binary", ran,
		"the generated composite never executed: "+firstLine(out))
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	trace, _ := m["trace_id"].(string)
	var txs []map[string]any
	_ = json.Unmarshal([]byte(k3.Get("eve", "/v1/transactions?limit=40")), &txs)
	inner := 0
	for _, t := range txs {
		if str(t, "parent_trace_id") == trace {
			inner++
		}
	}
	s.n.Check("compose.wasm_bought_another_action", inner > 0,
		"the composite ran but bought nothing, so no composition was measured")
	return nil
}

// ---- steps ------------------------------------------------------------------

func (s *story) actSteps() error {
	// A step parks a call: the process waits, someone else supplies the result, the call resumes.
	// It is a payment boundary that is not a network hop, and the rules about completing one twice
	// or from the wrong party are what this exercises.
	k1 := s.k("k1")
	var ids []string
	for i := 0; i < 4; i++ {
		out, _ := k1.Run("ana", "--json", "run", "sys/message",
			fmt.Sprintf(`{"to":"ben","message":"netsim step %d"}`, i))
		var m struct {
			Result struct {
				StepID string `json:"step_id"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(out), &m) == nil && m.Result.StepID != "" {
			ids = append(ids, m.Result.StepID)
		}
	}
	s.n.Check("step.parked_and_visible", len(ids) > 0, "no step was parked by sys/message")
	if len(ids) == 0 {
		return nil
	}
	s.n.MustWork("step.recipient_sees_it", k1, "ben", "step", "list")

	// A step belongs to the party it was parked for, and anyone else completing it is taking the
	// funds it holds. This is asked of a step that is still waiting: on one already completed the
	// kernel refuses because it is finished, which proves a different rule.
	s.n.MustRefuse("step.wrong_party_refused", "not found|denied|permitted|forbidden|only the step|required caller",
		k1, "ana", "step", "complete", ids[0], `{"echo":"stolen"}`)

	for _, id := range ids {
		s.n.MustWork("step.completed", k1, "ben", "step", "complete", id, `{"echo":"received"}`)
	}
	// A step is a payment boundary: completing one twice must settle once, and the replay must be
	// refused rather than pay again.
	s.n.MustRefuse("step.replay_settles_once", "already|complete|settled|not waiting|not found",
		k1, "ben", "step", "complete", ids[0], `{"echo":"again"}`)
	return nil
}

// ---- value transfers --------------------------------------------------------

func (s *story) actValue() error {
	// sys/transfer moves value as the result of a call rather than as an operator's instruction.
	k1, k4 := s.k("k1"), s.k("k4")
	before := k1.Balance("ben")
	moved := int64(0)
	for i := 0; i < 5; i++ {
		if s.n.MustWork("value.transferred", k1, "ana", "run", "sys/transfer",
			fmt.Sprintf(`{"target":"ben","amount":%s}`, s.px(20))) {
			moved += 20 * s.scale
		}
	}
	got := k1.Balance("ben") - before
	s.n.Check("value.recipient_credited", got == moved,
		fmt.Sprintf("five transfers of %d moved %d, not %d", 20*s.scale, got, moved))
	s.n.MustRefuse("value.overdraw_refused", shortOfMoney, k4, "gus",
		"run", "sys/transfer", fmt.Sprintf(`{"target":"fay","amount":%s}`, s.px(999999)))
	s.n.MustRefuse("value.unknown_target_refused", "not found|invalid|target|no such", k1, "ana",
		"run", "sys/transfer", fmt.Sprintf(`{"target":"nobody-here","amount":%s}`, s.px(1)))
	return nil
}

// ---- delegated authorization -----------------------------------------------

func (s *story) actDelegated() error {
	// Some actions act on the caller's own account elsewhere, and need the caller's credential
	// rather than the provider's. No funds may lock before consent exists, and withdrawing consent
	// takes effect at once.
	k2 := s.k("k2")
	auth := `{"scheme":"delegated_bearer","config":{"header":"X-Api-Key","template":"{token}"}}`
	s.n.MustWork("auth.published", k2, "cara", "action", "create", "vault/read", "--kind", "http",
		"--source", s.n.Backend+"/headers", "--price", cr(15),
		"--description", "Reads the caller's own upstream account", "--auth", auth)
	_, _ = k2.Run("cara", "action", "enable", "cara/vault/read")

	// A refusal for want of consent must cost nothing and count for nothing (U28), read off the
	// action rather than off its owner's balance: she is a seller and a buyer besides, and her
	// earlier trades are still being paid while this runs, so her balance moves for reasons that
	// have nothing to do with this call. Her action's record does not. Money moves only with a
	// transaction and a use is what a reputation is made of, so one number covers both — and the
	// call that follows, once consent exists, is what proves the number counts at all.
	s.n.MustRefuse("auth.refused_without_consent", "grant|connect|consent|authoriz",
		k2, "cara", "run", "cara/vault/read", `{"msg":"x"}`)
	used := k2.Num("cara", "uses", "action", "stats", "cara/vault/read")
	s.n.Check("auth.nothing_charged_before_consent", used == 0,
		fmt.Sprintf("a call refused for want of consent was recorded against the action (%d uses)", used))
	s.n.MustWork("auth.connected", k2, "cara", "user", "connect", "cara/vault", "--token", "netsim-delegated-token")
	s.n.MustWork("auth.works_with_consent", k2, "cara", "run", "cara/vault/read", `{"msg":"x"}`)
	s.n.Check("auth.consented_call_is_counted",
		k2.Num("cara", "uses", "action", "stats", "cara/vault/read") == 1,
		"the call that consent allowed was not recorded, so the check above counted nothing")
	host := strings.TrimPrefix(s.n.Backend, "http://")
	s.n.MustWork("auth.disconnected", k2, "cara", "user", "disconnect", "--account", "bearer:"+host)
	s.n.MustRefuse("auth.refused_after_revoking", "grant|connect|consent|authoriz",
		k2, "cara", "run", "cara/vault/read", `{"msg":"x"}`)
	return nil
}

// ---- ratings and evidence ---------------------------------------------------

func (s *story) actEvidence() error {
	// A rating is public evidence: a stranger deciding whether to buy must be able to read it, and
	// must not learn who wrote it.
	rate := func(kernel, user string) {
		body := s.k(kernel).Get(user, "/v1/transactions?limit=40")
		var txs []map[string]any
		if json.Unmarshal([]byte(body), &txs) != nil {
			return
		}
		done := 0
		for _, t := range txs {
			if t["status"] != "success" || t["rating"] != nil || done >= 8 {
				continue
			}
			done++
			value := "1"
			if done%4 == 0 {
				value = "0"
			}
			id, _ := t["id"].(string)
			_, _ = s.k(kernel).Run(user, "tx", "rate", id, value, "--note", fmt.Sprintf("netsim run %d", done))
		}
	}
	rate("k2", "cara")
	rate("k1", "ana")
	rate("k3", "dan")

	s.n.MustWork("evidence.ratings_readable", s.k("k1"), "ana", "action", "ratings", "ana/echo")
	s.n.MustWork("evidence.stats_readable", s.k("k1"), "ana", "action", "stats", "ana/echo")
	for _, pair := range [][2]string{{"k1", "k2"}, {"k2", "k1"}, {"k3", "k1"}} {
		s.n.MustWork("evidence.peer_inspectable", s.k(pair[0]), "sysop-"+pair[0],
			"admin", "peer", "inspect", "--", s.k(pair[1]).Key)
	}
	// The privacy check is made on what actually crosses the wire, not on what the command line
	// chose to print.
	s.receiptsVerify()
	s.write("ratings-projection.json", s.k("k1").Get("ana", "/v1/actions/ana%2Fecho/ratings"))
	s.write("catalogue-anonymous.json", s.k("k1").Get("", "/v1/actions"))
	return nil
}

func (s *story) write(name, body string) {
	_ = writeFile(filepath.Join(s.n.Root, name), body)
}

// ---- the late joiner --------------------------------------------------------

func (s *story) actLateJoiner() error {
	p := kernelPlan[4]
	k, err := s.join(p.name, p.creditLimit, "")
	if err != nil {
		return err
	}
	k.MakeUser("hal")
	if err := s.fund("k5", "hal", 6000); err != nil {
		return err
	}
	if err := s.awaitFunds(); err != nil {
		return err
	}
	s.publish("k5", "hal", "service", "public", "/echo", "A late provider's service")
	// A newcomer must be found by the kernels already running, and must be able to buy from them.
	found := poll(30*time.Second, 2*time.Second, func() bool {
		good, _ := s.buy("k1", "ana", "hal@late/service")
		return good
	})
	s.n.Check("late.discovered_and_bought_from", found,
		"a kernel that joined a running economy was never reachable")
	if good, needs := s.buy("k5", "hal", "cara@shop/quote"); needs {
		_ = s.settle("k5", "k2")
	} else if !good {
		s.n.Check("late.can_buy", false, "the late joiner could not buy from an established kernel")
	}
	return nil
}

// ---- churn ------------------------------------------------------------------

func (s *story) actChurn() error {
	// A kernel is killed outright, with no chance to tidy up, while others are trading with it.
	// What this can honestly observe is that nobody loses money and that trade resumes.
	//
	// It is NOT a mid-call crash. The provider is stopped before the call, so the caller's request
	// was never dispatched. The hard case — the request received and the answer lost — needs the
	// provider killed while it is executing, and is covered by flows/flows_federation.sh
	// (a real process, killed during a slow call) and cmd/juice/fedsim_test.go (deterministically,
	// with the response dropped). Claiming it here would be claiming a result this act cannot
	// produce.
	k2, k3 := s.k("k2"), s.k("k3")
	// The buyer whose money is watched is one that only ever buys. A user who also sells earns
	// while the window is open — from its own trades, and from obligations settling that have
	// nothing to do with the outage — and its total would say nothing about who gained from what.
	before := k3.Holdings("eve")
	k2.Stop()

	out, err := k3.Run("eve", "run", "cara@shop/quote", `{"msg":"gone"}`)
	s.n.Check("churn.call_to_an_offline_provider_does_not_succeed", err != nil,
		"a call to a kernel that had been killed reported success: "+firstLine(out))

	// A composite that reaches the dead kernel fails at its own level while the call it dispatched
	// is still unanswered. That child may have executed abroad, so the parent's failure must not
	// settle it (P7): the money stays reserved and the process stays open on it. A refund here
	// would be this kernel deciding, on no evidence, that a seller it cannot reach is owed nothing.
	openBefore := s.openProcesses(k3)
	_, _ = k3.Run("eve", "run", "dan/chain", `{"msg":"parent fails over a dead peer"}`)
	parkedAfter, _ := s.parkedFunds(k3)
	s.n.Check("churn.a_failed_parent_does_not_settle_its_dispatched_child",
		s.openProcesses(k3) >= openBefore || parkedAfter > 0,
		"a composite failing over an unreachable peer left no call awaiting its receipt, so the child was presumed dead")

	// Funds parked on an unreachable peer are what an operator needs to see (§13), so they are
	// read from the supervision view and reported with their age. Whether they should have been
	// released is the protocol's business, not this act's: U35 lets an ambiguously dispatched call
	// stay parked until proof arrives.
	parked, age := s.parkedFunds(k3)
	if parked > 0 {
		fmt.Printf("    %d parked on an unreachable peer, oldest %s\n", parked, age)
	}
	s.n.Check("churn.parked_funds_are_visible_to_the_operator",
		parked == 0 || age != "",
		"funds are parked but the supervision view does not say since when")

	restartedAt := time.Now()
	if _, err := s.n.Boot("k2", s.opts("k2", kernelPlan[1].creditLimit)); err != nil {
		return err
	}
	back := poll(90*time.Second, 3*time.Second, func() bool {
		good, needs := s.buy("k3", "dan", "cara@shop/quote")
		if needs {
			_ = s.settle("k3", "k2")
			good, _ = s.buy("k3", "dan", "cara@shop/quote")
		}
		return good
	})
	s.recoverySeconds = time.Since(restartedAt).Seconds()
	s.n.Check("churn.trade_resumes_after_a_restart", back,
		"trade never resumed with a kernel that came back")
	s.n.Check("churn.buyer_gained_nothing_from_the_outage", k3.Holdings("eve") <= before,
		fmt.Sprintf("a buyer held more after a provider's death than before it: %d then %d",
			before, k3.Holdings("eve")))
	return nil
}

// openProcesses counts the computations a kernel still holds funds inside. A process stays open
// while any call it made awaits a remote receipt, so this is what a presumed-dead child would close.
func (s *story) openProcesses(k *Kernel) int {
	procs, _ := pages(k, "sysop-"+k.Name, "/v1/processes")
	open := 0
	for _, p := range procs {
		if str(p, "status") == "open" {
			open++
		}
	}
	return open
}

// parkedFunds is what a kernel is holding on calls still waiting for a signed receipt, and how long
// the oldest has waited, from the operator's own supervision view (§13) — not from transaction
// rows, because a call that is still parked has no settled transaction to read.
func (s *story) parkedFunds(k *Kernel) (int64, string) {
	procs, _ := pages(k, "sysop-"+k.Name, "/v1/processes")
	_, funds, oldest := parkedIn(procs)
	return funds, oldest
}

// ---- attacks ----------------------------------------------------------------

func (s *story) actAttacks() error {
	s.attackSybil()
	s.attackFreeRider()
	s.attackReachingPastARefusal()
	s.attackSquatting()
	// The fifth is replay, which is made in the act where money enters: one payment submitted
	// three times must be credited once. It belongs there because that is where the payment is.
	return nil
}

// An attacker who can mint identities cheaply tries to draw more unsecured credit than one
// identity could, by spreading the borrowing over several. The answer is one global cap for all
// peers together, so the total owed stays under it however many identities appear.
func (s *story) attackSybil() {
	fmt.Println("  the attacker mints identities and borrows against all of them")
	fail := func(why string) { s.n.Check("attack.sybil_shares_one_cap", false, why) }
	for _, name := range sybilPlan {
		// Each attacker has its own account name (an actor's name is its client home) and pays its
		// own way: an identity with no money is refused for want of funds and never reaches the
		// cap, which is a different rule, tested elsewhere.
		k, err := s.join(name, 3000, "")
		if err != nil {
			fail("the attacking kernel would not start: " + err.Error())
			return
		}
		k.MakeUser("sybil-" + name)
		if err := s.fund(name, "sybil-"+name, 1500); err != nil {
			fail("could not fund the attacker: " + err.Error())
			return
		}
	}
	if err := s.awaitFunds(); err != nil {
		fail(err.Error())
		return
	}
	// The victim's books are cleared first — leaving the limit under work the honest economy already
	// ran up would start the attack past the line — and it is then put on a limit one small headroom
	// above where its trading has left it. The headroom, not any absolute number, is what the attack
	// is measured against: honest trading may leave the counter anywhere, including below zero when a
	// winning draw has paid more than was owed.
	s.settleAll([][2]string{{"k1", "k2"}, {"k3", "k2"}, {"k4", "k2"}, {"k5", "k2"}})
	before := s.exposureOf("k2")
	cap := before + sybilVictimHeadroom*s.scale
	if cap < 0 {
		cap = 0
	}
	victim := s.opts("k2", sybilVictimHeadroom)
	victim.CreditLimit = cap
	if err := s.reboot("k2", victim); err != nil {
		fail("the victim would not restart: " + err.Error())
		return
	}
	defer s.restoreVictim()

	// Both identities borrow as fast as they can, alongside an honest kernel, until the shop refuses
	// someone. Nobody has to withhold anything: a debt is paid only once the buyer's own worker has
	// sent the money and said so, and a burst of calls outruns that, which is the ordinary way an
	// attacker reaches a limit. The limit governs the total the shop has delivered and not been paid
	// for, not any one identity's share, and the refusal must be the credit engine's — a refusal for
	// want of the caller's own money would let an unfunded attack look like a defended one.
	//
	// What the attack is measured against is the absolute ceiling, not what it added. The guarantee
	// is that the shop's unpaid delivered work never passes its limit however many identities
	// appear; a delta cannot express it, because a draw that wins pays the whole face value, so cash
	// received can exceed what was owed and leave the counter below zero — against which any
	// increase reads as larger than the limit while the limit itself was never breached.
	refused := false
	for i := 0; i < 12; i++ {
		for _, b := range []struct{ kernel, user string }{{"k6", "sybil-k6"}, {"k7", "sybil-k7"}, {"k3", "dan"}} {
			out, err := s.k(b.kernel).Run(b.user, "--json", "run", "cara@shop/quote", `{"msg":"sybil"}`)
			refused = refused || (err != nil && (strings.Contains(out, "credit with peer") || strings.Contains(out, "exhausted")))
		}
	}
	after := s.exposureOf("k2")
	s.n.Check("attack.sybil_reached_the_cap", refused,
		"nobody was ever refused for want of credit, so the limit was never reached and the attack proves nothing")
	s.n.Check("attack.sybil_shares_one_cap", after <= cap,
		fmt.Sprintf("three identities carried the shop to %d of unpaid delivered work, past its one limit of %d (it began at %d)",
			after, cap, before))
}

// exposureOf is what a kernel has delivered to foreign buyers and not been paid for.
func (s *story) exposureOf(name string) int64 {
	if s.k(name) == nil {
		return 0
	}
	m, _ := read[map[string]any](s.k(name), "sysop-"+name, "admin", "kernel", "show")
	return num(m, "exposure")
}

// mustMap is the first value of a read that may have failed, so a check reports a zero rather than
// a panic when a kernel is unreachable.
func mustMap(m map[string]any, _ error) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// reboot restarts one kernel on new options over its own durable store, which is how the story
// changes a policy a kernel reads only at startup.
func (s *story) reboot(name string, o bootOpts) error {
	s.k(name).Stop()
	_, err := s.n.Boot(name, o)
	return err
}

// restoreVictim puts the shop back on the economy's own cap, so the acts that follow trade under
// the same terms as the acts before.
func (s *story) restoreVictim() {
	if err := s.reboot("k2", s.opts("k2", kernelPlan[1].creditLimit)); err != nil {
		s.n.Check("attack.sybil_victim_restored", false, err.Error())
		return
	}
	// A restarted kernel listens on a new port, so its peers must find it again. Without this the
	// next act's refusal is about a kernel that could not be reached, which is a different reason
	// from the one under test. A buyer that still owes the shop is refused for that instead, which
	// is also a different reason, so what it owes is settled before reachability is judged.
	if !poll(60*time.Second, 2*time.Second, func() bool {
		good, needs := s.buy("k3", "dan", "cara@shop/quote")
		if needs {
			_ = s.settle("k3", "k2")
			good, _ = s.buy("k3", "dan", "cara@shop/quote")
		}
		return good
	}) {
		s.n.Check("attack.sybil_victim_restored", false, "the shop was never reachable again after the attack")
	}
}

// A caller with nothing tries to have work done anyway. The refusal comes before anything is
// locked or executed, so the balance is untouched and no transaction exists.
func (s *story) attackFreeRider() {
	fmt.Println("  a caller with no balance asks for paid work")
	k1 := s.k("k1")
	k1.MakeUser("pauper")
	before := k1.Balance("pauper")
	s.n.MustRefuse("attack.freerider_local", shortOfMoney,
		k1, "pauper", "run", "ana/echo", `{"msg":"free"}`)
	s.n.MustRefuse("attack.freerider_remote", shortOfMoney+"|credit",
		k1, "pauper", "run", "cara@shop/quote", `{"msg":"free"}`)
	s.n.Check("attack.freerider_locked_nothing", k1.Balance("pauper") == before,
		"a caller with no balance had funds moved anyway")
}

// A receipt is the provider's signed statement that it did the work, and it must verify without
// asking anyone. What this suite can honestly show is that the check exists and passes on genuine
// receipts, over the same interface an operator uses.
//
// Forging one is deliberately NOT done here. Editing a stored receipt means writing to the
// kernel's private database, which reaches past every interface this suite is supposed to drive
// and binds it to a schema it has no business knowing. A forged receipt also has to be re-signed
// to test anything beyond "a corrupted row is rejected", and signing requires the kernel's keys.
// That test belongs where a peer's signature can be forged on the wire, and it is already there:
// kernel/federation_test.go covers a receipt with the wrong action, the wrong argument hash, a
// charge above the agreed price, a negative charge, and a reply hash that does not match.
func (s *story) receiptsVerify() {
	k1 := s.k("k1")
	var txs []map[string]any
	_ = json.Unmarshal([]byte(k1.Get("ben", "/v1/transactions?limit=40")), &txs)
	id := ""
	for _, t := range txs {
		if t["status"] == "success" && t["remote_receipt_json"] != nil {
			if v, ok := t["id"].(string); ok {
				id = v
				break
			}
		}
	}
	if id == "" {
		s.n.Check("evidence.receipt_verifies", false, "no receipted cross-kernel transaction existed to verify")
		return
	}
	// `tx verify` answers rather than fails: it prints the verdict and names each check, which is
	// what an operator needs, so this reads the answer instead of the exit status.
	out, _ := k1.Run("ben", "tx", "verify", id)
	s.n.Check("evidence.receipt_verifies", strings.Contains(out, "valid: true"),
		"a genuine receipt did not verify: "+firstLine(out))
	for _, check := range []string{"receipt_hash", "signature"} {
		s.n.Check("evidence.receipt_names_its_checks_"+check, strings.Contains(out, check),
			"the verification did not name the "+check+" check, so an operator cannot see what held")
	}
}

// Two attempts at the same thing: an action marked local is asked for from another kernel, and an
// action on a kernel the attacker cannot reach is asked for through one that can. Access is not
// transitive, and a kernel does not relay on request.
func (s *story) attackReachingPastARefusal() {
	fmt.Println("  the attacker asks for what was never exported")
	k3, k1 := s.k("k3"), s.k("k1")
	miss := "not found|not available|refused|denied|no such|unknown"
	s.n.MustRefuse("attack.local_action_not_exported", miss, k3, "dan", "run", "ana@hub/local-only", `{"msg":"x"}`)
	s.n.MustRefuse("attack.private_action_not_exported", miss, k3, "dan", "run", "ana@hub/helper", `{"msg":"x"}`)
	s.n.MustRefuse("attack.no_relay_through_a_third_kernel", miss, k3, "dan", "run", "ana@shop/echo", `{"msg":"x"}`)
	// Value may not name a beneficiary on another kernel: a transfer that crossed would let a
	// caller move a stranger's balance from outside.
	s.n.MustRefuse("attack.value_may_not_cross", "not found|local|invalid|target", k1, "ana",
		"run", "sys/transfer", fmt.Sprintf(`{"target":"cara@shop","amount":%s}`, s.px(5)))
}

// A newcomer advertises a handle a victim already uses for someone else. A petname is the local
// operator's own label and is never taken from the network, so the victim's name must still point
// where it did, and must still buy from the same kernel.
func (s *story) attackSquatting() {
	fmt.Println("  a newcomer claims a name already in use")
	if _, err := s.join("k8", 3000, "shop"); err != nil {
		s.n.Check("attack.squatted_name_unmoved", false, "the squatting kernel would not start")
		return
	}
	time.Sleep(5 * time.Second)
	out, _ := s.k("k1").Run("sysop-k1", "--json", "admin", "peer", "list", "--all")
	var rows []map[string]any
	_ = json.Unmarshal([]byte(out), &rows)
	pointsAt := ""
	for _, r := range rows {
		if r["petname"] == "shop" {
			pointsAt, _ = r["public_key"].(string)
		}
	}
	s.n.Check("attack.squatted_name_unmoved", pointsAt == s.k("k2").Key,
		"the victim's name for the shop moved to the squatter")
	// The name is only worth defending if it still buys from the right kernel.
	good, needs := s.buy("k3", "dan", "cara@shop/quote")
	if needs {
		_ = s.settle("k3", "k2")
		good, _ = s.buy("k3", "dan", "cara@shop/quote")
	}
	s.n.Check("attack.squatted_name_still_buys", good,
		"a purchase addressed to the squatted name no longer reached the original kernel")
}

// ---- settle everything ------------------------------------------------------

func (s *story) actSettleAll() error {
	names := []string{"k1", "k2", "k3", "k4", "k5"}
	names = append(names, sybilPlan...)
	var pairs [][2]string
	for _, d := range names {
		for _, c := range names {
			if d != c {
				pairs = append(pairs, [2]string{d, c})
			}
		}
	}

	// A buyer pays before it tells the seller which payment settles the obligation, so nothing can be
	// settled until the money it committed has actually moved. On a chain that is the chain's own
	// pace, which is why this waits rather than assuming.
	quiet := func() bool {
		for _, name := range names {
			if s.k(name) == nil {
				continue
			}
			m, _ := read[map[string]any](s.k(name), "sysop-"+name, "admin", "kernel", "show")
			sys, _ := m["sys"].(map[string]any)
			if num(sys, "pending_payouts") != 0 {
				return false
			}
		}
		return true
	}
	if !poll(s.n.Rail.SettleWait(), 2*time.Second, quiet) {
		fmt.Println("  some payments are still in flight")
	}

	// Settling is itself activity: closing one obligation lets the seller serve that buyer again, and
	// a retry landing during the pass opens one the pass has already looked at. So it repeats until
	// nothing anywhere is owed, bounded by what the rail needs to make a payment final — on a chain
	// the money is not the kernel's to see until the chain says so.
	done := 0
	settled := poll(s.n.Rail.SettleWait(), 3*time.Second, func() bool {
		d, _ := s.settleAll(pairs)
		done += d
		if d > 0 {
			fmt.Printf("  %d positions settled\n", d)
		}
		for _, c := range names {
			for _, b := range names {
				if b == c || s.k(c) == nil || s.k(b) == nil {
					continue
				}
				// A failed read is not an absence of debt, so it keeps the poll going and, if it
				// never succeeds, leaves the run saying obligations are still open.
				if n, err := s.owed(c, b); err != nil || n > 0 {
					return false
				}
			}
		}
		return true
	})
	fmt.Printf("  %d positions settled\n", done)
	if !settled {
		fmt.Println("  some obligations are still open")
	}

	return nil
}
