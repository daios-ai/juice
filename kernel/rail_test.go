// SPDX-License-Identifier: AGPL-3.0-only

package kernel_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/store"
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
	return kernel.RailDeposit{Key: "rail:ref:" + ref, TxHash: ref, Amount: amount}, nil
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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

	if _, attributed, err := k.SetBlockchainAddress(ctx, alice.ID, "0xALICE", "sig"); err != nil {
		t.Fatalf("register: %v", err)
	} else if len(attributed) != 1 {
		t.Fatalf("registering must deliver what that address already paid in, got %d", len(attributed))
	}
	if avail, _ := balanceOf(t, st, alice.ID); avail != 120 {
		t.Errorf("balance after registering: got %d, want 120", avail)
	}
	// The canonical form is what is stored, so a differently-cased duplicate collides.
	bob := setupUser(t, st, "bob", 0)
	if _, _, err := k.SetBlockchainAddress(ctx, bob.ID, "0xalice", "sig"); !errors.Is(err, kernel.ErrInvalidInput) {
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
		NetworkFingerprint: "0000000000000000000000000000000000000000000000000000000000000000"}
	if _, err := k.AccumulateGossip(ctx, foreign, ""); err == nil {
		t.Error("a reply from another network must not be accumulated")
	}
	// A reply that names no network is not ours either: an omitted fingerprint is not a passport
	// (P9). Before this rule a peer could be indexed simply by leaving the field out.
	silent := &kernel.GossipResponse{PublicKey: "k", Handle: "peer"}
	if _, err := k.AccumulateGossip(ctx, silent, ""); err == nil {
		t.Error("a reply naming no network must not be accumulated")
	}

	fr.verifyErr = kernel.ErrUnauthorized.Wrap("signature was not made by that address")
	unproven := &kernel.GossipResponse{PublicKey: "k", Handle: "peer",
		NetworkFingerprint: testNet.Fingerprint, BlockchainAddress: "0xsomeone", BlockchainProof: "bad"}
	if _, err := k.AccumulateGossip(ctx, unproven, ""); err == nil {
		t.Error("a peer's unproven blockchain address must not be accumulated")
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

// announcedOwed puts a seller-side obligation in the state the buyer has said it paid, so a test can
// drive the half of settlement that waits for the money to actually arrive. It goes through the same
// admission and commit the kernel uses, because those are what the obligation is read off.
func announcedOwed(t *testing.T, st kernel.Store, id, peerID, sellerID, from, txHash string, amount int64) *kernel.Owed {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	rec := &kernel.IdempotencyRecord{ID: uuid.NewString(), IdempotencyKey: id, CounterpartyUserID: peerID, CreatedAt: now}
	if _, err := st.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	p := &kernel.Process{ID: uuid.NewString(), OwnerUserID: sellerID, Status: kernel.ProcessOpen, CreatedAt: now}
	tr := &kernel.Trace{ID: uuid.NewString(), ProcessID: p.ID, ActionOwnerID: sellerID, ActionID: "a",
		CallerUserID: peerID, IdempotencyRecordID: &rec.ID, OwedBlockchainAddress: from, CreatedAt: now,
		DispatchJSON: kernel.ServingRecordForTest(0, 0, amount, "0a0b", "cm", id, "peer-key")}
	// The execution itself is free here so the seller's balance stays what each test set it to; the
	// obligation is read off the receipt's charge, which is what the buyer owes.
	if err := st.BeginRun(ctx, p, tr, sellerID, 0, amount, amount*100); err != nil {
		t.Fatal(err)
	}
	tx := &kernel.Transaction{ID: uuid.NewString(), ProcessID: p.ID, TraceID: tr.ID, OwnerUserID: sellerID,
		CallerUserID: peerID, TargetUserID: sellerID, ActionID: "a", Status: kernel.TxSuccess,
		StartedAt: now, EndedAt: now}
	receipt := &kernel.Receipt{ID: uuid.NewString(), IssuerUserID: sellerID, TxID: tx.ID, TraceID: tr.ID,
		ActionID: "a", Status: kernel.TxSuccess, Charge: amount, Nonce: "0a0b", CreatedAt: now}
	if err := st.CommitCall(ctx, tx, receipt, tr.ID, p.ID, kernel.CallerProcess, sellerID, "", 0, 0, nil, rec.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyReveal(ctx, "", tr.ID, amount, txHash, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ReadOwed(ctx, id, peerID)
	return got
}

// Both books name the obligation by the same word. The buyer wrote it on the trace it dispatched
// under; the seller was admitted under the peer's key, which lives on the record its trace points
// at — so a seller reading its own transaction can still name the payment that closes it.
func TestASellersTransactionNamesItsObligation(t *testing.T) {
	k, st, _, _ := railFixture(t)
	ctx := context.Background()
	peer := peerWithAddress(t, k, st, "kpeerTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT", "0xdebtor")
	seller := setupUser(t, st, "seller", 0)
	announcedOwed(t, st, "tk-view", peer.ID, seller.ID, "0xdebtor", "0xpaid-view", 40)

	txs, err := st.ListTransactions(ctx, kernel.TxFilter{PartyUserID: seller.ID})
	if err != nil || len(txs) == 0 {
		t.Fatalf("transactions: %d %v", len(txs), err)
	}
	v, err := k.ReadTransaction(ctx, seller.ID, txs[0].ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if v.TicketID != "tk-view" {
		t.Fatalf("the seller must name its obligation: got %q, want tk-view", v.TicketID)
	}
	if again, _ := st.ReadOwed(ctx, v.TicketID, peer.ID); again == nil {
		t.Error("the name the transaction gives must be the name the obligation answers to")
	}
}

// An obligation closes only against a payment from the buyer's own proven address. An equal payment
// from anybody else, even in the very transaction the buyer named, closes nothing.
func TestTicketMatchesOnlyTheBuyersOwnPayment(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	peer := peerWithAddress(t, k, st, "kpeerAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "0xdebtor")
	seller := setupUser(t, st, "seller", 0)
	announcedOwed(t, st, "tk-1", peer.ID, seller.ID, "0xdebtor", "0xpaid", 40)

	fr.deposits = []kernel.RailDeposit{{Key: "rail:0xpaid:0", TxHash: "0xpaid", From: "0xstranger", Amount: 40, Block: 1}}
	k.RailPass(ctx)
	if got, _ := st.ReadOwed(ctx, "tk-1", peer.ID); got.Status != kernel.OwedAnnounced {
		t.Fatalf("a stranger's payment closed the obligation: %s", got.Status)
	}

	fr.deposits = append(fr.deposits, kernel.RailDeposit{Key: "rail:0xpaid:1", TxHash: "0xpaid", From: "0xdebtor", Amount: 40, Block: 1})
	k.RailPass(ctx)
	if got, _ := st.ReadOwed(ctx, "tk-1", peer.ID); got.Status != kernel.OwedCredited {
		t.Fatalf("the buyer's own payment must close the obligation: %s", got.Status)
	}
	if avail, _ := balanceOf(t, st, seller.ID); avail != 40 {
		t.Errorf("the seller must be credited what it was owed: %d", avail)
	}
	if avail, _ := balanceOf(t, st, peer.ID); avail != 0 {
		t.Errorf("a peer row holds no money: %d", avail)
	}
	_ = sys
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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
func TestTicketMatchesWithinAMultiPaymentTransaction(t *testing.T) {
	k, st, fr, _ := railFixture(t)
	ctx := context.Background()
	peer := peerWithAddress(t, k, st, "kpeerCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "0xdebtor")
	seller := setupUser(t, st, "seller", 0)
	announcedOwed(t, st, "tk-3", peer.ID, seller.ID, "0xdebtor", "0xshared", 40)
	fr.deposits = []kernel.RailDeposit{
		{Key: "rail:0xshared:0", TxHash: "0xshared", From: "0xdebtor", Amount: 40, Block: 1},
		{Key: "rail:0xshared:1", TxHash: "0xshared", From: "0xother", Amount: 40, Block: 1},
	}
	k.RailPass(ctx)
	if got, _ := st.ReadOwed(ctx, "tk-3", peer.ID); got.Status != kernel.OwedCredited {
		t.Fatalf("the buyer's payment shares a transaction with another and must still close it: %s", got.Status)
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

// flakyRevealer is a federation client whose reveal channel can be down, so a draw already decided
// can fail to reach the seller and must be sent again.
type flakyRevealer struct {
	*fakeFederationHTTP
	down    bool
	reveals int
}

func (f *flakyRevealer) Reveal(_ context.Context, _ string, p kernel.RevealPayload, _ string) error {
	f.reveals++
	if f.down {
		return kernel.ErrPeerUnreachable.Wrap("peer is away")
	}
	f.revealed = append(f.revealed, p)
	return nil
}

// A losing draw the seller never heard about leaves it owed forever, so the reveal is the buyer's
// obligation and the worker keeps sending it until the peer has taken it.
func TestALostRevealIsSentAgain(t *testing.T) {
	st := newTestStore(t)
	sys := setupSys(t, nil, st)
	cfg := testConfig()
	cfg.FeeRecipientID = sys.ID
	fed := &flakyRevealer{fakeFederationHTTP: &fakeFederationHTTP{}, down: true}
	k := newKernel(cfg, kernel.Dependencies{Store: st, Logger: log.Discard(), Federation: fed})
	k.SetRail(newFakeRail())
	ctx := context.Background()
	if err := st.SetConfig(ctx, "signing_public_key", "test-kernel-key"); err != nil {
		t.Fatal(err)
	}
	peer := peerWithAddress(t, k, st, "kpeerDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD", "0xcreditor")
	buyer := setupUser(t, st, "buyer", 0)

	// A draw that lost: no payment was made, and the seller still has to be told so.
	trace := dispatchedCall(t, st, buyer.ID, peer.ID, "tk-lost", "aa")

	k.RailPass(ctx)
	k.RevealPending(ctx)
	if fed.reveals == 0 {
		t.Fatal("the worker never tried to tell the seller")
	}
	if revealedFlag(t, st, trace) {
		t.Fatal("an unacknowledged reveal must leave the call waiting to be told")
	}

	fed.down = false
	k.RevealPending(ctx)
	if !revealedFlag(t, st, trace) {
		t.Fatal("once the seller has heard, the draw is finished")
	}

	// And it stops: a finished obligation is not announced forever.
	before := fed.reveals
	k.RevealPending(ctx)
	if fed.reveals != before {
		t.Errorf("a closed obligation was announced again (%d → %d)", before, fed.reveals)
	}
}

// dispatchedCall stages what the buyer keeps of a remote call it has settled: the trace holding the
// secret it committed to, and the transaction naming the peer it bought from and what it owes. Those
// two records are the whole buy-side memory of a draw — there is no separate obligation row on this
// side — and the transaction's net is the obligation, which is what makes the call revealable.
func dispatchedCall(t *testing.T, st kernel.Store, buyerID, peerID, key, secret string) string {
	t.Helper()
	ctx := context.Background()
	p := &kernel.Process{ID: uuid.NewString(), OwnerUserID: buyerID, Status: kernel.ProcessOpen, CreatedAt: time.Now().UTC()}
	tr := &kernel.Trace{ID: uuid.NewString(), ProcessID: p.ID, CallerUserID: buyerID,
		IdempotencyKey: &key, DispatchJSON: kernel.DispatchRecordForTest(11, 11, 0, 0, 100, secret),
		CreatedAt: time.Now().UTC()}
	if err := st.BeginRun(ctx, p, tr, buyerID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := st.(*store.DB).ExecForTest(ctx,
		`INSERT INTO transactions (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,
		   target_user_id,action_id,status,gross,net,started_at,ended_at)
		 VALUES (?,?,?,'',?,?,?,'a','success',11,11,?,?)`,
		uuid.NewString(), p.ID, tr.ID, buyerID, buyerID, peerID, now, now); err != nil {
		t.Fatal(err)
	}
	return tr.ID
}

// revealedFlag reports whether the seller has acknowledged this call's draw.
func revealedFlag(t *testing.T, st kernel.Store, traceID string) bool {
	t.Helper()
	var n int64
	if err := st.(*store.DB).QueryRowForTest(context.Background(),
		`SELECT revealed FROM traces WHERE id=?`, traceID, &n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// A won draw is not announced until its payment is final: a seller told of a payment that never
// confirms would be owed forever.
func TestAWonDrawIsAnnouncedOnlyOnceItsPaymentIsFinal(t *testing.T) {
	st := newTestStore(t)
	sys := setupSys(t, nil, st)
	cfg := testConfig()
	cfg.FeeRecipientID = sys.ID
	fed := &flakyRevealer{fakeFederationHTTP: &fakeFederationHTTP{}}
	k := newKernel(cfg, kernel.Dependencies{Store: st, Logger: log.Discard(), Federation: fed})
	fr := newFakeRail()
	fr.stall = true // the payment is submitted but not yet final
	k.SetRail(fr)
	ctx := context.Background()
	if err := st.SetConfig(ctx, "signing_public_key", "test-kernel-key"); err != nil {
		t.Fatal(err)
	}
	peer := peerWithAddress(t, k, st, "kpeerEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", "0xcreditor")
	buyer := setupUser(t, st, "buyer", 0)
	if _, err := k.Deposit(ctx, sys.ID, buyer.ID, 100, "", "earn"); err != nil {
		t.Fatal(err)
	}
	dispatchedCall(t, st, buyer.ID, peer.ID, "tk-won", "aa")
	pay := &kernel.RailTransfer{ID: "tk-won", Kind: kernel.RailKindObligation, Party: buyer.ID,
		Destination: "0xcreditor", Amount: 100, Credit: 100,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := st.ReserveRailTransfer(ctx, sys.ID, pay); err != nil {
		t.Fatal(err)
	}

	k.RailPass(ctx)
	k.RevealPending(ctx)
	if fed.reveals != 0 {
		t.Fatalf("a payment still in flight was announced %d times", fed.reveals)
	}

	fr.mu.Lock()
	fr.status["tk-won"] = kernel.RailConfirmed
	fr.mu.Unlock()
	k.RailPass(ctx)
	k.RevealPending(ctx)
	if fed.reveals != 1 {
		t.Fatalf("a final payment must be announced exactly once, got %d", fed.reveals)
	}
	if len(fed.revealed) != 1 || fed.revealed[0].TxHash == "" {
		t.Errorf("the announcement must name the payment: %+v", fed.revealed)
	}
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
	if _, _, err := k.SetBlockchainAddress(ctx, alice.ID, "0xalice", "sig"); err != nil {
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

// What admission counts against the credit limit is the most the call can owe, the serving markup
// included — not the bare price. Reserving the price alone would let every admitted call carry its
// markup past the limit, so a buyer could always draw more unpaid work than the operator allowed.
func TestTheCreditLimitCountsTheWholeObligation(t *testing.T) {
	st := newTestStore(t)
	econ := kernel.DefaultEconomy()
	econ.CreditLimit, econ.RemoteBPS = 1000, 500 // a 5% markup on top of the price
	k := newKernel(testConfig(), kernel.Dependencies{Store: st, Economy: econ})
	ctx := context.Background()
	sys := setupSys(t, k, st)
	seller := setupUser(t, st, "seller", 10000)
	peer := peerWithAddress(t, k, st, "kpeerFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", "0xbuyer")
	k.RegisterNativeHandler("quote", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	act := setupAction(t, st, seller.ID, "quote", 100)
	act.Visibility = kernel.VisibilityPublic
	if err := st.UpdateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	if _, err := k.RunFederated(ctx, peer.ID, mustResolve(t, k, ctx, seller.ID, "quote"), map[string]any{}, "", kernel.BuyerTerms{Commitment: "cm"}); err != nil {
		t.Fatalf("RunFederated: %v", err)
	}
	// The call charged 100 and the seller's own markup adds 5, so the buyer owes 105 and that is
	// what the kernel has delivered unpaid.
	got, err := k.Exposure(ctx, sys.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 105 {
		t.Errorf("exposure after one 100-credit call at a 5%% markup = %d, want 105", got)
	}
}

// A payment the rail signed and that then reverted is presented again, because the debt did not go
// away — but never under the same name: a settled rail operation cannot be asked twice, so reusing
// it would ask nothing at all forever. The money stays committed, unlike a withdrawal, which gives
// its money back. And the rail must not be left halted: a blocked row stops every withdrawal and
// every obligation payment on the kernel, so a reverted ticket that parked there would take the
// whole rail down with no way to clear it.
func TestARevertedObligationPaymentIsPresentedAgainUnderAFreshName(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	buyer := setupUser(t, st, "buyer", 0)
	if _, err := k.Deposit(ctx, sys.ID, buyer.ID, 100, "", "earn"); err != nil {
		t.Fatal(err)
	}
	pay := &kernel.RailTransfer{ID: "won-1", Kind: kernel.RailKindObligation, Party: buyer.ID,
		Destination: "0xseller", Amount: 40, Credit: 40,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := st.ReserveRailTransfer(ctx, sys.ID, pay); err != nil {
		t.Fatal(err)
	}
	// Submitted, then mined and reverted.
	fr.stall = true
	k.RailPass(ctx)
	first, _ := st.ReadRailTransfer(ctx, "won-1")
	presented := fr.pays
	fr.mu.Lock()
	fr.status[first.RailOp()] = kernel.RailFailed
	fr.mu.Unlock()
	k.RailPass(ctx)

	row, _ := st.ReadRailTransfer(ctx, "won-1")
	if row.Attempt == first.Attempt {
		t.Fatalf("a reverted payment was not given a fresh attempt: still %d", row.Attempt)
	}
	if row.RailOp() == first.RailOp() {
		t.Fatalf("the fresh attempt reuses the spent name %q", row.RailOp())
	}
	if a, _ := balanceOf(t, st, buyer.ID); a != 60 {
		t.Errorf("a debt that failed to pay is still a debt: the buyer has %d, want 60", a)
	}
	// The rail is still running: nothing about a reverted ticket halts withdrawals.
	if reason, _, _ := k.RailStop(ctx); reason != "" {
		t.Errorf("a reverted ticket halted the whole rail: %q", reason)
	}
	// And it was genuinely presented again rather than parked, in the same pass that saw the revert.
	if fr.pays <= presented {
		t.Errorf("the payment was never presented again (%d attempts, was %d)", fr.pays, presented)
	}
	if row.Status != kernel.RailStatusSubmitted {
		t.Errorf("the fresh attempt is not in flight: %s", row.Status)
	}
}

// Where a buyer pays from is proven and frozen when its call is admitted, not learned later: an
// unproven address is refused, a priced call from a buyer proving none is refused on a world with
// addresses, and an accepted one names the payer on the obligation from that moment — before any
// payment can land, and for a buyer this kernel had never pulled.
func TestABuyersPayerIsProvenAndFrozenAtAdmission(t *testing.T) {
	k, st, fr, sys := railFixture(t)
	ctx := context.Background()
	seller := setupUser(t, st, "seller", 10000)
	peer := peerWithAddress(t, k, st, "kpeerHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHHH", "")
	k.RegisterNativeHandler("quote", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	act := setupAction(t, st, seller.ID, "quote", 100)
	act.Visibility = kernel.VisibilityPublic
	if err := st.UpdateAction(ctx, act); err != nil {
		t.Fatal(err)
	}
	run := func(terms kernel.BuyerTerms) error {
		_, err := k.RunFederated(ctx, peer.ID, mustResolve(t, k, ctx, seller.ID, "quote"), map[string]any{}, "", terms)
		return err
	}

	free := setupAction(t, st, seller.ID, "free", 0)
	free.Visibility = kernel.VisibilityPublic
	if err := st.UpdateAction(ctx, free); err != nil {
		t.Fatal(err)
	}
	k.RegisterNativeHandler("free", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return map[string]any{}, nil
	})

	fr.verifyErr = errors.New("bad proof")
	if err := run(kernel.BuyerTerms{Commitment: "cm", BlockchainAddress: "0xbuyer", BlockchainProof: "forged"}); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Fatalf("an unproven payer must be refused, got %v", err)
	}
	// A free call owes nothing, names no payer, and is verified against nothing — even by a rail
	// that would reject an empty address if asked.
	if _, err := k.RunFederated(ctx, peer.ID, mustResolve(t, k, ctx, seller.ID, "free"), map[string]any{}, "", kernel.BuyerTerms{}); err != nil {
		t.Fatalf("a free call must not need a payer: %v", err)
	}
	// It records which request it answers, as every admitted call does, and no ticket: there is
	// no draw to hold a nonce for and no payer to name (P4, P10).
	var withTicket int64
	if err := st.(*store.DB).QueryRowForTest(ctx,
		`SELECT COUNT(*) FROM traces WHERE action_id=?
		   AND (COALESCE(json_extract(dispatch_json,'$.nonce'),'') <> '' OR owed_blockchain_address <> '')`,
		free.ID, &withTicket); err != nil {
		t.Fatal(err)
	}
	if withTicket != 0 {
		t.Error("a call that owes nothing froze ticket terms")
	}
	var named int64
	if err := st.(*store.DB).QueryRowForTest(ctx,
		`SELECT COUNT(*) FROM traces WHERE action_id=?
		   AND COALESCE(json_extract(dispatch_json,'$.counterparty'),'') <> ''`, free.ID, &named); err != nil {
		t.Fatal(err)
	}
	if named != 1 {
		t.Error("an admitted call must record the buyer it answers, priced or not")
	}
	fr.verifyErr = nil
	if err := run(kernel.BuyerTerms{Commitment: "cm"}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Fatalf("a priced call naming no payer must be refused on a world with addresses, got %v", err)
	}
	if err := run(kernel.BuyerTerms{Commitment: "cm", BlockchainAddress: "0xBUYER", BlockchainProof: "ok"}); err != nil {
		t.Fatalf("a proven payer must be admitted: %v", err)
	}
	var id string
	if err := st.(*store.DB).QueryRowForTest(ctx,
		`SELECT owed_blockchain_address FROM traces WHERE caller_user_id=? AND owed_blockchain_address <> ''`, peer.ID, &id); err != nil {
		t.Fatalf("no trace froze the payer: %v", err)
	}
	if id != "0xbuyer" {
		t.Errorf("frozen payer = %q, want the canonical form the rail verified", id)
	}
	_ = sys
}

// A seller that answers "I have no such obligation" has repudiated it. No message can make it
// accept one, so the reveal is retired instead of being retried forever ahead of reveals that can
// still succeed (P10). Repudiation is the seller's act and is logged as such; the buyer keeps the
// money it did not have to pay.
func TestARepudiatedRevealIsRetired(t *testing.T) {
	st := newTestStore(t)
	sys := setupSys(t, nil, st)
	cfg := testConfig()
	cfg.FeeRecipientID = sys.ID
	fed := &repudiatingRevealer{fakeFederationHTTP: &fakeFederationHTTP{}}
	k := newKernel(cfg, kernel.Dependencies{Store: st, Logger: log.Discard(), Federation: fed})
	k.SetRail(newFakeRail())
	ctx := context.Background()
	if err := st.SetConfig(ctx, "signing_public_key", "test-kernel-key"); err != nil {
		t.Fatal(err)
	}
	peer := peerWithAddress(t, k, st, "kpeerRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRRR", "0xcreditor")
	buyer := setupUser(t, st, "repudiated-buyer", 0)
	trace := dispatchedCall(t, st, buyer.ID, peer.ID, "tk-repudiated", "aa")

	k.RailPass(ctx)
	k.RevealPending(ctx)
	if fed.reveals != 1 {
		t.Fatalf("the seller was told %d times, want once", fed.reveals)
	}
	if !revealedFlag(t, st, trace) {
		t.Fatal("a repudiated obligation must be retired, not retried forever")
	}
	k.RevealPending(ctx)
	if fed.reveals != 1 {
		t.Errorf("a retired obligation was announced again (%d attempts)", fed.reveals)
	}
}

// repudiatingRevealer is a seller that denies the obligation exists.
type repudiatingRevealer struct {
	*fakeFederationHTTP
	reveals int
}

func (f *repudiatingRevealer) Reveal(_ context.Context, _ string, _ kernel.RevealPayload, _ string) error {
	f.reveals++
	return kernel.ErrNotFound.Wrap("no such obligation")
}
