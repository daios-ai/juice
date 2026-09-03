package kernel_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
)

// fakeRail is a rail whose every outcome the test chooses, so the money rules can be exercised
// against a payment that stalls, reverts, buys fuel, or is refused, without a chain.
type fakeRail struct {
	mu        sync.Mutex
	addr      string
	notReady  error
	pays      int
	outcome   kernel.RailOutcome
	stall     bool // the payment is submitted but not yet final, as a chain payment is
	payErr    error
	status    map[string]kernel.RailStatus
	deposits  []kernel.RailDeposit
	scanned   uint64
	refill    int64
	refillSt  kernel.RailStatus
	verifyErr error
	// needRefill makes the next Pay say fuel is short; refills counts purchases; found is what
	// FindRefill answers, the purchase a crash may have interrupted; refillErr is fuel the operator
	// cannot afford.
	needRefill  bool
	refills     int // purchases actually made
	refillCalls int // times the kernel asked for fuel, presentations of one purchase included
	found       *kernel.RailRefill
	refillErr   error
	atSigning   func() // what the books must look like when the rail is asked to sign a purchase
}

func newFakeRail() *fakeRail {
	return &fakeRail{addr: "0xvault", status: map[string]kernel.RailStatus{}, refillSt: kernel.RailConfirmed}
}

func (f *fakeRail) Ready(context.Context) error { return f.notReady }
func (f *fakeRail) Address() string             { return f.addr }

func (f *fakeRail) Destination(registered string) (string, error) {
	if f.addr == "" {
		return "", nil
	}
	if registered == "" {
		return "", kernel.ErrInvalidState.Wrap("no payment address is registered")
	}
	return registered, nil
}

func (f *fakeRail) Pay(_ context.Context, id, _ string, _ int64) (kernel.RailOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pays++
	if f.payErr != nil {
		return kernel.RailOutcome{}, f.payErr
	}
	if f.needRefill {
		return kernel.RailOutcome{NeedRefill: true}, nil
	}
	out := f.outcome
	if out.TxHash == "" && out.Blocked == "" {
		out.TxHash = "tx:" + id
	}
	if out.TxHash != "" && !f.stall {
		f.status[id] = kernel.RailConfirmed
	} else if out.TxHash != "" {
		f.status[id] = kernel.RailPending
	}
	return out, nil
}

func (f *fakeRail) Outcome(_ context.Context, id string) (kernel.RailStatus, kernel.RailFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.status[id]
	if !ok {
		return kernel.RailUnknown, kernel.RailFact{}, nil
	}
	return st, kernel.RailFact{TxHash: "tx:" + id, Exec: st == kernel.RailConfirmed}, nil
}

// RefillCost also says when the purchase stops being outstanding: a rail carries it until it
// resolves, and a resolved one is no longer what the next call would present.
func (f *fakeRail) RefillCost(context.Context, string) (int64, kernel.RailStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refillSt == kernel.RailConfirmed || f.refillSt == kernel.RailFailed {
		f.found = nil
	}
	return f.refill, f.refillSt, nil
}

// Refill mirrors the adaptor: a rail carries one purchase at a time, so while one is outstanding
// the same one is reported — presented again, never duplicated.
func (f *fakeRail) Refill(_ context.Context, _ int64) (kernel.RailRefill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refillCalls++
	if f.refillErr != nil {
		return kernel.RailRefill{}, f.refillErr
	}
	if f.found != nil {
		return *f.found, nil
	}
	if f.atSigning != nil {
		f.atSigning()
	}
	f.refills++
	f.needRefill = false
	r := kernel.RailRefill{ID: fmt.Sprintf("refill-%d", f.refills), Max: 40}
	f.found = &r
	return r, nil
}

// FindRefill offers the purchase this rail holds until the ledger has a row for it, which is where
// a real walk down the nonces stops too.
func (f *fakeRail) FindRefill(_ context.Context, known func(string) bool) (kernel.RailRefill, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.found == nil || known(f.found.ID) {
		return kernel.RailRefill{}, false, nil
	}
	return *f.found, true, nil
}

func (f *fakeRail) ScanDeposits(_ context.Context, since uint64) ([]kernel.RailDeposit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanned = since
	var out []kernel.RailDeposit
	for _, d := range f.deposits {
		if d.Block >= since {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeRail) Witness(_ context.Context, ref string, amount int64) (kernel.RailDeposit, error) {
	for _, d := range f.deposits {
		if d.TxHash == ref {
			return d, nil
		}
	}
	if amount <= 0 {
		return kernel.RailDeposit{}, kernel.ErrInvalidInput.Wrap("amount must be positive")
	}
	return kernel.RailDeposit{Key: "rail:ref:" + ref, TxHash: "ref:" + ref, Amount: amount}, nil
}

func (f *fakeRail) FinalizedBalances(context.Context) (int64, string, uint64, bool, error) {
	return 0, "", 0, false, nil
}
func (f *fakeRail) DepositsScannedTo() (uint64, bool, error) { return f.scanned, true, nil }
func (f *fakeRail) Sign([]byte) (string, error)              { return "proof", nil }

func (f *fakeRail) Verify(_ []byte, address, _ string) (string, error) {
	if f.verifyErr != nil {
		return "", f.verifyErr
	}
	return strings.ToLower(address), nil
}

// railFixture is a kernel with a rail whose outcomes the test controls, plus a funded operator.
func railFixture(t *testing.T) (*kernel.Kernel, kernel.Store, *fakeRail, *kernel.Account) {
	t.Helper()
	st := newTestStore(t)
	cfg := testConfig()
	// In production the operator's authority and the operator's money are one account. The
	// fixture keeps it that way, or a refill would draw on an account nobody funded.
	sys := setupSys(t, nil, st)
	cfg.FeeRecipientID = sys.ID
	k := newKernel(cfg, kernel.Dependencies{Store: st, Logger: log.Discard()})
	fr := newFakeRail()
	k.SetRail(fr)
	// A registration signature names this kernel, so the fixture needs the identity a real boot
	// would have written.
	if err := st.SetConfig(context.Background(), "signing_public_key", "test-kernel-key"); err != nil {
		t.Fatal(err)
	}
	return k, st, fr, sys
}

func balanceOf(t *testing.T, st kernel.Store, id string) (int64, int64) {
	t.Helper()
	u, err := st.ReadUser(context.Background(), id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return u.Available, u.Locked
}

// A crossing names the payment it records, and naming the same one twice moves money once. Without
// that rule a retried command would mint credits (U3).
func TestDepositNamesItsPaymentAndIsIdempotent(t *testing.T) {
	k, st, _, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)

	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "invoice", ""); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("a deposit naming no payment must be refused, got %v", err)
	}
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "invoice", "inv-1"); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "invoice", "inv-1"); err != nil {
		t.Fatalf("replaying one payment must succeed: %v", err)
	}
	if got, _ := balanceOf(t, st, alice.ID); got != 500 {
		t.Fatalf("balance after recording one payment twice: got %d, want 500", got)
	}
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 900, "invoice", "inv-1"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("the same payment on other terms must be refused, got %v", err)
	}
}

// The solvency identity holds after ordinary funding: what users hold is exactly what came in.
func TestSolvencyHoldsAfterFunding(t *testing.T) {
	k, st, _, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	rep, err := k.RailInspect(ctx, sys.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Gap != 0 {
		t.Errorf("gap after funding: got %d, want 0 (%+v)", rep.Gap, rep.Position)
	}
	if rep.Position.Vault != 500 {
		t.Errorf("vault: got %d, want 500", rep.Position.Vault)
	}
}

// A withdrawal takes the money out of reach at once, pays, and crosses out. The books close with no
// gap, and the money was never spendable in between (U51).
func TestWithdrawalReservesPaysAndCrossesOut(t *testing.T) {
	k, st, _, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}

	row, err := k.Withdraw(ctx, alice.ID, uuid.NewString(), 200, "cash out")
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if row.Destination != "0xalice" {
		t.Errorf("destination must be snapshotted, got %q", row.Destination)
	}
	if avail, _ := balanceOf(t, st, alice.ID); avail != 300 {
		t.Errorf("balance after withdrawing: got %d, want 300", avail)
	}
	fresh, err := st.ReadRailTransfer(ctx, row.ID)
	if err != nil || fresh == nil {
		t.Fatalf("read row: %v", err)
	}
	if fresh.Status != kernel.RailStatusConfirmed {
		t.Fatalf("status after a rail that confirms at once: got %q", fresh.Status)
	}
	if _, locked := balanceOf(t, st, sys.ID); locked != 0 {
		t.Errorf("nothing may stay held once the payment is final, got %d", locked)
	}
	rep, _ := k.RailInspect(ctx, sys.ID)
	if rep.Gap != 0 || rep.Position.Vault != 300 {
		t.Errorf("after paying out: gap=%d vault=%d, want 0 and 300", rep.Gap, rep.Position.Vault)
	}
}

// A payment that finalizes without moving money returns exactly what it reserved, once, and leaves
// the original entries untouched.
func TestFailedWithdrawalCompensatesOnce(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}
	fr.stall = true // submitted, not yet final — the state a chain payment sits in
	row, err := k.Withdraw(ctx, alice.ID, uuid.NewString(), 200, "")
	if err != nil {
		t.Fatal(err)
	}
	if avail, _ := balanceOf(t, st, alice.ID); avail != 300 {
		t.Fatalf("the money must leave reach as soon as the payment is authorized, got %d", avail)
	}
	// The payment turns out to have reverted; the worker observes it and gives the money back.
	fr.mu.Lock()
	fr.status[row.ID] = kernel.RailFailed
	fr.mu.Unlock()
	k.RailPass(ctx)
	k.RailPass(ctx)

	if avail, _ := balanceOf(t, st, alice.ID); avail != 500 {
		t.Errorf("a failed payment must return the money exactly once: got %d, want 500", avail)
	}
	rep, _ := k.RailInspect(ctx, sys.ID)
	if rep.Gap != 0 {
		t.Errorf("gap after compensation: got %d", rep.Gap)
	}
}

// A rail that refuses to sign stops outgoing money and says why, while everything else carries on —
// the fees that keep accruing are what let the operator clear it (D23).
func TestBlockedPaymentHaltsOutgoingWorkOnly(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}
	fr.outcome = kernel.RailOutcome{Blocked: "stablecoin too low, top up"}
	if _, err := k.Withdraw(ctx, alice.ID, uuid.NewString(), 100, ""); err != nil {
		t.Fatal(err)
	}
	reason, _, err := k.RailStop(ctx)
	if err != nil || reason == "" {
		t.Fatalf("a refused payment must halt outgoing work: %q %v", reason, err)
	}
	// Another withdrawal is refused under its own error, not the caller's own shortage.
	_, err = k.Withdraw(ctx, alice.ID, uuid.NewString(), 100, "")
	if !errors.Is(err, kernel.ErrRailStopped) {
		t.Errorf("second withdrawal: want ErrRailStopped, got %v", err)
	}
	// Money still comes in while outgoing work is stopped.
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 10, "", "inv-2"); err != nil {
		t.Errorf("deposits must continue while outgoing payments are stopped: %v", err)
	}
	// The shortage clears; the row is presented again and the halt lifts with it.
	fr.outcome = kernel.RailOutcome{}
	k.RailPass(ctx)
	if reason, _, _ := k.RailStop(ctx); reason != "" {
		t.Errorf("the halt must lift when the last blocked payment does, still %q", reason)
	}
}

// The rail records a payment before the kernel books it, so a pass that dies in between must find it
// again. The kernel keeps its own mark for exactly that reason.
func TestUnbookedPaymentIsFoundOnTheNextPass(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}
	fr.deposits = []kernel.RailDeposit{{Key: "rail:tx-9", TxHash: "tx-9", From: "0xalice", Amount: 70, Block: 5}}
	k.RailPass(ctx)
	if avail, _ := balanceOf(t, st, alice.ID); avail != 70 {
		t.Fatalf("a payment from a registered sender must be delivered: got %d", avail)
	}
	// A second pass sees the same payment again and moves nothing.
	k.RailPass(ctx)
	if avail, _ := balanceOf(t, st, alice.ID); avail != 70 {
		t.Errorf("re-observing one payment moved money twice: got %d, want 70", avail)
	}
	if _, err := k.RailInspect(ctx, sys.ID); err != nil {
		t.Fatal(err)
	}
}

// Money from a sender nobody has registered waits for the operator, and registering the address
// later delivers it — attribution is a fact about the address, not about when we learned it.
func TestRegisteringAnAddressAttributesEarlierPayments(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	fr.deposits = []kernel.RailDeposit{{Key: "rail:tx-1", TxHash: "tx-1", From: "0xalice", Amount: 120, Block: 2}}
	k.RailPass(ctx)

	held, err := k.ListHeldDeposits(ctx, sys.ID)
	if err != nil || len(held) != 1 {
		t.Fatalf("an unrecognized sender must be held and listed: %d %v", len(held), err)
	}
	if avail, _ := balanceOf(t, st, alice.ID); avail != 0 {
		t.Fatalf("nothing may be credited before the sender is known, got %d", avail)
	}

	if _, attributed, err := k.SetRailAddress(ctx, alice.ID, "0xALICE", "sig"); err != nil {
		t.Fatalf("register: %v", err)
	} else if len(attributed) != 1 {
		t.Fatalf("registering must deliver what that address already paid in, got %d", len(attributed))
	}
	if avail, _ := balanceOf(t, st, alice.ID); avail != 120 {
		t.Errorf("balance after registering: got %d, want 120", avail)
	}
	// The canonical form is what is stored, so a differently-cased duplicate collides.
	bob := setupUser(t, st, "bob", 0)
	if _, _, err := k.SetRailAddress(ctx, bob.ID, "0xalice", "sig"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("one address, one account: got %v", err)
	}
}

// An unverified rail stops the whole pass, not merely the money verbs: decimals and token identity
// are unknown until it passes, and a payment booked under the wrong ones is off by orders of
// magnitude.
func TestUnverifiedRailStopsEverything(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	fr.notReady = errors.New("endpoint unreachable")
	fr.deposits = []kernel.RailDeposit{{Key: "rail:tx-1", TxHash: "tx-1", From: "0xalice", Amount: 50, Block: 1}}

	k.RailPass(ctx)
	if avail, _ := balanceOf(t, st, alice.ID); avail != 0 {
		t.Errorf("an unverified rail must book nothing, got %d", avail)
	}
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 10, "", "inv-1"); !errors.Is(err, kernel.ErrRailStopped) {
		t.Errorf("money verbs must wait for verification: got %v", err)
	}
}

// A peer that merely declares an address could name a stranger's and claim their payment, so gossip
// carrying an unproven one is not accumulated. A reply from another network is refused outright.
func TestGossipRefusesForeignNetworkAndUnprovenAddress(t *testing.T) {
	k, _, fr, _ := railFixture(t)
	ctx := context.Background()

	foreign := &kernel.GossipResponse{PublicKey: "k", Handle: "peer",
		NetworkDigest: "0000000000000000000000000000000000000000000000000000000000000000"}
	if _, err := k.AccumulateGossip(ctx, foreign, ""); err == nil {
		t.Error("a reply from another network must not be accumulated")
	}

	fr.verifyErr = kernel.ErrUnauthorized.Wrap("signature was not made by that address")
	unproven := &kernel.GossipResponse{PublicKey: "k", Handle: "peer",
		NetworkDigest: testNet.Digest, RailAddress: "0xsomeone", RailProof: "bad"}
	if _, err := k.AccumulateGossip(ctx, unproven, ""); err == nil {
		t.Error("a peer's unproven rail address must not be accumulated")
	}
}

// The solvency identity is what says the books are whole. Breaking one record by hand must show up
// as a gap rather than pass unnoticed, and the report must name the term that moved.
func TestSolvencyNamesABrokenRecord(t *testing.T) {
	k, st, _, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if rep, _ := k.RailInspect(ctx, sys.ID); rep.Gap != 0 {
		t.Fatalf("the books must start whole, gap=%d", rep.Gap)
	}

	// Credits appear that no payment brought in — the shape of a bug, or of theft.
	if err := st.(interface {
		ExecForTest(context.Context, string, ...any) error
	}).ExecForTest(ctx, `UPDATE accounts SET available = available + 250 WHERE id = ?`, alice.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := k.RailInspect(ctx, sys.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Gap != 250 {
		t.Errorf("gap after inventing 250 credits: got %d, want 250", rep.Gap)
	}
	if rep.Position.Liabilities != 750 || rep.Position.Vault != 500 {
		t.Errorf("the report must name which term moved: liabilities=%d vault=%d",
			rep.Position.Liabilities, rep.Position.Vault)
	}
}

// peerWithAddress provisions a peer account whose kernel has proved it is paid from addr.
func peerWithAddress(t *testing.T, k *kernel.Kernel, st kernel.Store, key, addr string) *kernel.Account {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertKernel(ctx, key, "peer", "", addr, "proof", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	peer, err := k.EnsureKernelAccount(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return peer
}

// A claim closes only against a payment from the debtor's own proven address. An equal payment
// from anybody else, even in the very transaction the debtor named, closes nothing.
func TestClaimMatchesOnlyTheDebtorsOwnPayment(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	peer := peerWithAddress(t, k, st, "kpeerAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "0xdebtor")
	// The peer owes nothing yet; a claim is what it says it paid us.
	claim := &kernel.RailTransfer{ID: "sid-1", Kind: kernel.RailKindClaim, Party: peer.ID,
		Amount: 40, Credit: 40, TxHash: "0xpaid", Status: kernel.RailStatusAnnounced, CreatedAt: time.Now().UTC()}
	if err := st.CreateRailTransfer(ctx, claim); err != nil {
		t.Fatal(err)
	}

	fr.deposits = []kernel.RailDeposit{{Key: "rail:0xpaid:0", TxHash: "0xpaid", From: "0xstranger", Amount: 40, Block: 1}}
	k.RailPass(ctx)
	if row, _ := st.ReadRailTransfer(ctx, "sid-1"); row.Status != kernel.RailStatusAnnounced {
		t.Fatalf("a stranger's payment closed the claim: %s", row.Status)
	}

	fr.deposits = append(fr.deposits, kernel.RailDeposit{Key: "rail:0xpaid:1", TxHash: "0xpaid", From: "0xdebtor", Amount: 40, Block: 1})
	k.RailPass(ctx)
	if row, _ := st.ReadRailTransfer(ctx, "sid-1"); row.Status != kernel.RailStatusCredited {
		t.Fatalf("the debtor's own payment must close the claim: %s", row.Status)
	}
	if avail, _ := balanceOf(t, st, peer.ID); avail != 40 {
		t.Errorf("the debt must be credited to the peer's row: %d", avail)
	}
	_ = sys
}

// The operator's record of a peer's payment names the transaction the peer announced, never the
// settlement's own id, which is not a fact anything outside could witness.
func TestOperatorClosesAClaimAgainstTheNamedPayment(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	peer := peerWithAddress(t, k, st, "kpeerBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "0xdebtor")
	claim := &kernel.RailTransfer{ID: "sid-2", Kind: kernel.RailKindClaim, Party: peer.ID,
		Amount: 40, Credit: 40, TxHash: "0xpaid2", Status: kernel.RailStatusAnnounced, CreatedAt: time.Now().UTC()}
	if err := st.CreateRailTransfer(ctx, claim); err != nil {
		t.Fatal(err)
	}
	fr.deposits = []kernel.RailDeposit{{Key: "rail:0xpaid2:0", TxHash: "0xpaid2", From: "0xdebtor", Amount: 40, Block: 1}}

	if _, err := k.Deposit(ctx, sys.ID, peer.ID, 40, "", "sid-2"); err != nil {
		t.Fatalf("operator record: %v", err)
	}
	dep, _ := st.ReadRailTransfer(ctx, "rail:0xpaid2:0")
	if dep == nil || dep.Status != kernel.RailStatusCredited {
		t.Fatalf("the witnessed payment must be the announced transaction, got %+v", dep)
	}
	if row, _ := st.ReadRailTransfer(ctx, "sid-2"); row.Status != kernel.RailStatusCredited {
		t.Errorf("claim: %s", row.Status)
	}
}

// A payment from an unknown sender waits held; the operator names its transaction and its owner,
// and it is delivered once — with no second delivery on a repeat and no crash on the reply.
func TestOperatorAttributesAHeldPayment(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	fr.deposits = []kernel.RailDeposit{{Key: "rail:0xin:0", TxHash: "0xin", From: "0xunknown", Amount: 90, Block: 1}}
	k.RailPass(ctx)
	if avail, _ := balanceOf(t, st, alice.ID); avail != 0 {
		t.Fatalf("held money was delivered to nobody's account: %d", avail)
	}
	for i := 0; i < 2; i++ {
		if _, err := k.Deposit(ctx, sys.ID, alice.ID, 90, "", "0xin"); err != nil {
			t.Fatalf("attribute (pass %d): %v", i, err)
		}
	}
	if avail, _ := balanceOf(t, st, alice.ID); avail != 90 {
		t.Errorf("attributed %d, want 90 exactly once", avail)
	}
	if _, locked := balanceOf(t, st, sys.ID); locked != 0 {
		t.Errorf("the hold must end on delivery, still %d", locked)
	}
}

// Money nobody has claimed must never hide the money that is on its way out.
func TestHeldPaymentsCannotStarveTheWorker(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		fr.deposits = append(fr.deposits, kernel.RailDeposit{Key: fmt.Sprintf("rail:0xdust:%d", i),
			TxHash: "0xdust", From: "0xnobody", Amount: 1, Block: 1})
	}
	k.RailPass(ctx)
	fr.stall = true
	row, err := k.Withdraw(ctx, alice.ID, uuid.NewString(), 100, "")
	if err != nil {
		t.Fatal(err)
	}
	fr.mu.Lock()
	fr.status[row.ID] = kernel.RailConfirmed
	fr.mu.Unlock()
	k.RailPass(ctx)
	if fresh, _ := st.ReadRailTransfer(ctx, row.ID); fresh.Status != kernel.RailStatusConfirmed {
		t.Errorf("a withdrawal behind 600 held payments never reached the worker: %s", fresh.Status)
	}
}

// A refill is recorded before the rail signs it, and a refill the rail made just before a crash is
// adopted rather than lost: the ledger never learns of fuel it did not pay for.
func TestRefillIsRecordedBeforeSigningAndAdoptedAfterACrash(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, sys.ID, 100, "", "earn"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}
	fr.needRefill = true
	fr.refillSt = kernel.RailPending
	// The books are ahead of the rail: when it is asked to sign, everything the operator could
	// spend is already held, so nothing can draw on the reserve it was told to keep.
	fr.atSigning = func() {
		if a, l := balanceOf(t, st, sys.ID); a != 0 || l != 110 {
			t.Errorf("at signing the operator's balance must be wholly held: available=%d locked=%d", a, l)
		}
	}
	row, err := k.Withdraw(ctx, alice.ID, uuid.NewString(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ReadRailTransfer(ctx, row.ID)
	fuel, _ := st.ReadRailTransfer(ctx, fresh.RefillID)
	if fresh.Status != kernel.RailStatusRefilling || fuel == nil || fuel.RefillID != "refill-1" || fr.refills != 1 {
		t.Fatalf("refill not recorded: status=%s fuel=%+v refills=%d", fresh.Status, fuel, fr.refills)
	}
	if a, l := balanceOf(t, st, sys.ID); a != 60 || l != 50 {
		t.Fatalf("the authorized maximum must leave the operator's balance: %d/%d", a, l)
	}

	// A crash between the rail signing and the kernel binding the purchase to its lock: the lock
	// still holds everything the operator could spend, the purchase is nobody's on the books, and
	// the payment is still waiting. The next pass adopts what the rail holds instead of buying a
	// second time, and settles the lock at the purchase's maximum.
	exec := st.(interface {
		ExecForTest(context.Context, string, ...any) error
	}).ExecForTest
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE rail_transfers SET refill_id='', amount=100 WHERE id=?`, []any{fuel.ID}},
		{`UPDATE accounts SET available=available-60, locked=locked+60 WHERE id=?`, []any{sys.ID}},
	} {
		if err := exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	k.RailPass(ctx)
	fuel, _ = st.ReadRailTransfer(ctx, fresh.RefillID)
	if fuel.RefillID != "refill-1" || fuel.Amount != 40 || fr.refills != 1 {
		t.Fatalf("the interrupted refill must be adopted, not repeated: fuel=%+v refills=%d", fuel, fr.refills)
	}
	if a, l := balanceOf(t, st, sys.ID); a != 60 || l != 50 {
		t.Fatalf("the lock must settle at the purchase's maximum: %d/%d", a, l)
	}

	// The purchase finalizes: the exact cost is booked, the payment goes out.
	fr.refill, fr.refillSt = 25, kernel.RailConfirmed
	k.RailPass(ctx)
	fresh, _ = st.ReadRailTransfer(ctx, row.ID)
	if fresh.Status != kernel.RailStatusConfirmed {
		t.Errorf("after the refill: %s", fresh.Status)
	}
	rep, _ := k.RailInspect(ctx, sys.ID)
	if rep.Gap != 0 {
		t.Errorf("gap after a refill cycle: %d", rep.Gap)
	}
}

// Fuel the operator cannot buy stops outgoing money like any other shortage: the payment is blocked
// with the reason, which is what the halt is made of, rather than waiting on a refill forever.
func TestUnaffordableFuelBlocksThePayment(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	// Fuel is the operator's own cost, so the operator must hold enough to buy it.
	if _, err := k.Deposit(ctx, sys.ID, sys.ID, 100, "", "earn"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}
	fr.needRefill = true
	fr.refillErr = kernel.ErrRailStopped.Wrap("not enough stablecoin to buy fuel")
	row, err := k.Withdraw(ctx, alice.ID, uuid.NewString(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ReadRailTransfer(ctx, row.ID)
	if fresh.Status != kernel.RailStatusBlocked || fresh.Reason == "" {
		t.Fatalf("unaffordable fuel must block the payment: status=%s reason=%q", fresh.Status, fresh.Reason)
	}
	if reason, _, err := k.RailStop(ctx); err != nil || reason == "" {
		t.Errorf("the halt must be visible to the operator: %q %v", reason, err)
	}
	// Once fuel is affordable again the same row goes out; nothing was bought twice.
	fr.refillErr = nil
	fr.refill, fr.refillSt = 25, kernel.RailConfirmed
	k.RailPass(ctx)
	if fresh, _ = st.ReadRailTransfer(ctx, row.ID); fresh.Status != kernel.RailStatusConfirmed {
		t.Errorf("after fuel became affordable: %s", fresh.Status)
	}
	if fr.refills != 1 {
		t.Errorf("fuel bought %d times, want once", fr.refills)
	}
}

// Two payments in one transaction are two facts. A claim is matched by the transaction and the
// debtor's proven address together, so another sender's payment in the same transaction cannot
// shadow it.
func TestClaimMatchesWithinAMultiPaymentTransaction(t *testing.T) {
	k, st, fr, _ := railFixture(t)
	ctx := context.Background()
	peer := peerWithAddress(t, k, st, "kpeerCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "0xdebtor")
	claim := &kernel.RailTransfer{ID: "sid-3", Kind: kernel.RailKindClaim, Party: peer.ID,
		Amount: 40, Credit: 40, TxHash: "0xshared", Status: kernel.RailStatusAnnounced, CreatedAt: time.Now().UTC()}
	if err := st.CreateRailTransfer(ctx, claim); err != nil {
		t.Fatal(err)
	}
	fr.deposits = []kernel.RailDeposit{
		{Key: "rail:0xshared:0", TxHash: "0xshared", From: "0xdebtor", Amount: 40, Block: 1},
		{Key: "rail:0xshared:1", TxHash: "0xshared", From: "0xother", Amount: 40, Block: 1},
	}
	k.RailPass(ctx)
	if row, _ := st.ReadRailTransfer(ctx, "sid-3"); row.Status != kernel.RailStatusCredited {
		t.Fatalf("the debtor's payment shares a transaction with another and must still close the claim: %s", row.Status)
	}
	if other, _ := st.ReadRailTransfer(ctx, "rail:0xshared:1"); other.Status != kernel.RailStatusHeld {
		t.Errorf("the other sender's payment must stay held: %s", other.Status)
	}
}

// The custody audit says whether it ran. Without a finalized read, or with a scan that has not
// reached the cut, it reports unchecked rather than fine.
func TestCustodyReportsWhetherItWasChecked(t *testing.T) {
	k, _, _, sys := railFixture(t)
	rep, err := k.RailInspect(context.Background(), sys.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CustodyChecked || rep.CustodyOK {
		t.Errorf("no finalized read was possible, yet custody reads checked=%v ok=%v", rep.CustodyChecked, rep.CustodyOK)
	}
}

// flakyAnnouncer is a federation client whose settlement channel can be down, so the announcement
// of a payment already made can fail and must be retried.
type flakyAnnouncer struct {
	*fakeFederationHTTP
	down      bool
	announces int
}

func (f *flakyAnnouncer) Settle(_ context.Context, _, kind, _, _, _ string, _ int64, _, _ string, _ []byte) (int, []byte, error) {
	if kind == "announce" {
		f.announces++
	}
	if f.down {
		return 0, nil, kernel.ErrPeerUnreachable.Wrap("peer is away")
	}
	return 200, []byte(`{"status":"announced"}`), nil
}

// A settlement paid but not yet announced is not finished: the creditor's books are still open.
// The worker keeps announcing until the peer has heard, whatever happened in between.
func TestPaidSettlementIsAnnouncedUntilThePeerHears(t *testing.T) {
	st := newTestStore(t)
	sys := setupSys(t, nil, st)
	cfg := testConfig()
	cfg.FeeRecipientID = sys.ID
	fed := &flakyAnnouncer{fakeFederationHTTP: &fakeFederationHTTP{}, down: true}
	k := newKernel(cfg, kernel.Dependencies{Store: st, Logger: log.Discard(), Federation: fed})
	k.SetRail(newFakeRail())
	ctx := context.Background()
	if err := st.SetConfig(ctx, "signing_public_key", "test-kernel-key"); err != nil {
		t.Fatal(err)
	}
	peer := peerWithAddress(t, k, st, "kpeerDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD", "0xcreditor")
	// We owe the peer 11, and hold enough of our own to pay it.
	if _, err := k.Deposit(ctx, sys.ID, sys.ID, 100, "", "earn"); err != nil {
		t.Fatal(err)
	}
	if err := st.(interface {
		ExecForTest(context.Context, string, ...any) error
	}).ExecForTest(ctx, `UPDATE accounts SET available = 11 WHERE id = ?`, peer.ID); err != nil {
		t.Fatal(err)
	}

	out, err := k.SettlePeer(ctx, sys.ID, peer.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	sid, _ := out["settlement_id"].(string)
	row, _ := st.ReadRailTransfer(ctx, sid)
	if row.Status != kernel.RailStatusConfirmed || fed.announces != 1 {
		t.Fatalf("paid with the peer away: status=%s announces=%d", row.Status, fed.announces)
	}

	// The peer is still away: the worker tries again, and the row stays open.
	k.RailPass(ctx)
	if row, _ = st.ReadRailTransfer(ctx, sid); row.Status != kernel.RailStatusConfirmed || fed.announces != 2 {
		t.Fatalf("still away: status=%s announces=%d", row.Status, fed.announces)
	}
	// The peer returns: the next pass tells it, and only then is the settlement finished.
	fed.down = false
	k.RailPass(ctx)
	if row, _ = st.ReadRailTransfer(ctx, sid); row.Status != kernel.RailStatusAnnounced || fed.announces != 3 {
		t.Fatalf("peer back: status=%s announces=%d", row.Status, fed.announces)
	}
	k.RailPass(ctx)
	if fed.announces != 3 {
		t.Errorf("an announced settlement must not be announced again: %d", fed.announces)
	}
}

// settleRouter carries one kernel's settlement rounds to another and remembers them, so a test can
// present a round again exactly as its author signed it — which is what a debtor grinding for a
// better draw would do.
type settleRouter struct {
	*fakeFederationHTTP
	creditor  *kernel.Kernel
	debtorKey string
	opens     []settleRound
	finishes  []string // settlement ids the debtor finished, in order
	loseReply bool     // deliver the finish to the creditor, then lose its answer on the way back
}

type settleRound struct {
	ts, sig, id string
	amount      int64
}

func (r *settleRouter) Settle(ctx context.Context, _, kind, ts, sig, id string, amount int64, nonce, txHash string, record []byte) (int, []byte, error) {
	if kind == "open" {
		r.opens = append(r.opens, settleRound{ts: ts, sig: sig, id: id, amount: amount})
	}
	status, body, err := r.creditor.HandleSettle(ctx, r.debtorKey, kind, ts, sig, id, amount, nonce, txHash, record)
	if kind == "finish" {
		r.finishes = append(r.finishes, id)
		if r.loseReply {
			return 0, nil, kernel.ErrPeerUnreachable.Wrap("the answer was lost on the way back")
		}
	}
	return status, body, err
}

// twoKernels builds a creditor owed d by a debtor that reaches it over a settleRouter.
func twoKernels(t *testing.T, d, Q int64) (kC, kD *kernel.Kernel, stC, stD kernel.Store, sysD *kernel.Account, peerOnC, peerOnD *kernel.Account, creditorKey string, router *settleRouter) {
	t.Helper()
	ctx := context.Background()
	stC = newTestStore(t)
	sysC := setupSys(t, nil, stC)
	cfgC := testConfig()
	cfgC.FeeRecipientID, cfgC.SettlementQuantum = sysC.ID, Q
	kC = newKernel(cfgC, kernel.Dependencies{Store: stC, Logger: log.Discard()})
	kC.SetRail(newFakeRail())
	if _, err := kC.Deposit(ctx, sysC.ID, sysC.ID, 1000, "", "earn"); err != nil {
		t.Fatal(err)
	}
	stD = newTestStore(t)
	sysD = setupSys(t, nil, stD)
	cfgD := testConfig()
	cfgD.FeeRecipientID, cfgD.SettlementQuantum = sysD.ID, Q
	router = &settleRouter{fakeFederationHTTP: &fakeFederationHTTP{}, creditor: kC}
	kD = newKernel(cfgD, kernel.Dependencies{Store: stD, Logger: log.Discard(), Federation: router})
	kD.SetRail(newFakeRail())
	if _, err := kD.Deposit(ctx, sysD.ID, sysD.ID, 100, "", "earn"); err != nil {
		t.Fatal(err)
	}
	debtorKey := publicKeyOf(cfgD)
	creditorKey = publicKeyOf(cfgC)
	router.debtorKey = debtorKey
	for _, id := range []struct {
		st  kernel.Store
		key string
	}{{stC, creditorKey}, {stD, debtorKey}} {
		if err := id.st.SetConfig(ctx, "signing_public_key", id.key); err != nil {
			t.Fatal(err)
		}
	}
	peerOnC = peerWithAddress(t, kC, stC, debtorKey, "0xdebtor")
	peerOnD = peerWithAddress(t, kD, stD, creditorKey, "0xcreditor")
	setBalance(t, stC, peerOnC.ID, -d)
	setBalance(t, stD, peerOnD.ID, d)
	return
}

// A draw is written down before its first round and finished by its own id. When the creditor's
// answer to the finish is lost, the creditor has already committed an outcome; the debtor must
// then come back for that same draw, never start another, and the two ledgers must agree.
func TestALostFinishReplyResumesTheSameDraw(t *testing.T) {
	const d, Q = 3, 4
	ctx := context.Background()
	_, kD, stC, stD, sysD, peerOnC, peerOnD, creditorKey, router := twoKernels(t, d, Q)

	router.loseReply = true
	if _, err := kD.SettlePeer(ctx, sysD.ID, creditorKey); err == nil {
		t.Fatal("a lost answer must surface as an error")
	}
	// The debtor remembers exactly one draw, still in progress, and has moved no money on it.
	draws, _ := stD.ListRailTransfers(ctx, kernel.RailKindSettlement, peerOnD.ID, kernel.RailStatusDrawing, 10)
	if len(draws) != 1 || len(router.finishes) != 1 || draws[0].ID != router.finishes[0] {
		t.Fatalf("the draw must be on record under the id the creditor answered: %d draws, finishes=%v", len(draws), router.finishes)
	}
	if a, l := balanceOf(t, stD, sysD.ID); a != 100 || l != 0 {
		t.Fatalf("a draw in progress moves nothing: %d/%d", a, l)
	}

	// Trying again finishes that draw: the creditor answers the same finish with the same record.
	router.loseReply = false
	res, err := kD.SettlePeer(ctx, sysD.ID, creditorKey)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(router.finishes) != 2 || router.finishes[1] != router.finishes[0] || len(router.opens) != 1 {
		t.Fatalf("the same draw must be finished, not a new one opened: opens=%d finishes=%v", len(router.opens), router.finishes)
	}
	stored, _ := stC.ReadSettlementRecord(ctx, router.finishes[0])
	var outcome kernel.SettlementRecord
	if err := json.Unmarshal([]byte(stored), &outcome); err != nil {
		t.Fatalf("the creditor's record: %q %v", stored, err)
	}
	// Whichever way the draw went, both ledgers say the same thing.
	debtorOwes, _ := balanceOf(t, stD, peerOnD.ID)
	creditorIsOwed, _ := balanceOf(t, stC, peerOnC.ID)
	switch outcome.Outcome {
	case "clear":
		if debtorOwes != 0 || creditorIsOwed != 0 || res["outcome"] != "clear" {
			t.Errorf("cleared: debtor row %d, creditor row %d, result %v", debtorOwes, creditorIsOwed, res)
		}
		if left, _ := stD.ListRailTransfers(ctx, kernel.RailKindSettlement, peerOnD.ID, "", 10); len(left) != 0 {
			t.Errorf("a cleared draw leaves no row: %+v", left[0])
		}
	case "pay":
		row, _ := stD.ReadRailTransfer(ctx, router.finishes[0])
		if row == nil || row.Status == kernel.RailStatusDrawing || debtorOwes != 0 || creditorIsOwed != -d {
			t.Errorf("payable: row %+v, debtor row %d, creditor row %d", row, debtorOwes, creditorIsOwed)
		}
		if pending, _ := stC.HasPendingSettlement(ctx, peerOnC.ID); !pending {
			t.Error("the creditor must be expecting the payment")
		}
	default:
		t.Fatalf("no outcome on the creditor: %+v", outcome)
	}
}

func setBalance(t *testing.T, st kernel.Store, id string, available int64) {
	t.Helper()
	exec := st.(interface {
		ExecForTest(context.Context, string, ...any) error
	}).ExecForTest
	if err := exec(context.Background(), `UPDATE accounts SET available=? WHERE id=?`, available, id); err != nil {
		t.Fatal(err)
	}
}

// A debt is drawn for once. The draw is fair only if losing it settles the matter: a debtor allowed
// to open a second settlement on a position it has already lost would keep drawing under fresh
// identifiers until a clear came up, and walk away having paid nothing for what it owed.
func TestADebtIsDrawnForOnce(t *testing.T) {
	const d, Q = 3, 4
	ctx := context.Background()
	kC, kD, stC, stD, sysD, peerOnC, peerOnD, creditorKey, router := twoKernels(t, d, Q)
	debtorKey := router.debtorKey

	// Draw until the debtor loses. A clear ends the debt, so the position is restored to try again;
	// once a draw is payable the debt stands until it is paid, which is the state under test.
	var payable bool
	for i := 0; i < 40 && !payable; i++ {
		setBalance(t, stC, peerOnC.ID, -d)
		setBalance(t, stD, peerOnD.ID, d)
		res, err := kD.SettlePeer(ctx, sysD.ID, creditorKey)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		payable = res["outcome"] != "clear"
	}
	if !payable {
		t.Fatal("no payable draw in forty attempts; the outcome function is not drawing")
	}

	// The debtor presents its own opening round again, signature and all. The debt is still there,
	// so nothing but the standing settlement stands in the way.
	last := router.opens[len(router.opens)-1]
	status, _, err := kC.HandleSettle(ctx, debtorKey, "open", last.ts, last.sig, last.id, last.amount, "", "", nil)
	if err != nil || status != 409 {
		t.Fatalf("a second draw on a debt already lost: status=%d err=%v, want 409", status, err)
	}
}

// publicKeyOf is a kernel's own public key as its peers name it.
func publicKeyOf(cfg kernel.Config) string {
	return base64.RawURLEncoding.EncodeToString(cfg.SigningKey.Public().(ed25519.PublicKey))
}

// A purchase can be durable and still never reach the chain, and the rail then refuses every later
// operation until it resolves — so a payment waiting on fuel would wait forever. Each pass presents
// the purchase again, which is idempotent, rather than buying a second one.
func TestAStalledPurchaseIsPresentedAgainNotRepeated(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	if _, err := k.Deposit(ctx, sys.ID, sys.ID, 100, "", "earn"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Deposit(ctx, sys.ID, alice.ID, 500, "", "inv-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.SetRailAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
		t.Fatal(err)
	}
	fr.needRefill = true
	fr.refillSt = kernel.RailPending // the purchase never finalizes
	row, err := k.Withdraw(ctx, alice.ID, uuid.NewString(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	presented := fr.refillCalls
	k.RailPass(ctx)
	k.RailPass(ctx)
	if fr.refills != 1 {
		t.Errorf("fuel bought %d times, want once", fr.refills)
	}
	if fr.refillCalls <= presented {
		t.Errorf("a purchase that never went out was never presented again: %d calls", fr.refillCalls)
	}
	if fresh, _ := st.ReadRailTransfer(ctx, row.ID); fresh.Status != kernel.RailStatusRefilling {
		t.Errorf("the payment must still be waiting on its fuel: %s", fresh.Status)
	}
	// Once the purchase finalizes the payment goes out, and nothing was bought twice.
	fr.refill, fr.refillSt = 25, kernel.RailConfirmed
	k.RailPass(ctx)
	if fresh, _ := st.ReadRailTransfer(ctx, row.ID); fresh.Status != kernel.RailStatusConfirmed {
		t.Errorf("after the purchase finalized: %s", fresh.Status)
	}
	if fr.refills != 1 {
		t.Errorf("fuel bought %d times, want once", fr.refills)
	}
}
