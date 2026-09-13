package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

// railFixture seeds the operator and one ordinary account, which is all any rail movement needs.
func railFixture(t *testing.T) (*DB, string, string) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	sys, alice := newUser("sys", 0), newUser("alice", 0)
	for _, u := range []*kernel.Account{sys, alice} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatalf("create %s: %v", u.Handle, err)
		}
	}
	return db, sys.ID, alice.ID
}

// balances reads an account's two numbers: what it can spend, and what is promised elsewhere.
func balances(t *testing.T, db *DB, id string) (int64, int64) {
	t.Helper()
	u, err := db.ReadUser(context.Background(), id)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	return u.Available, u.Locked
}

func depositRow(id, from string, amount int64) *kernel.RailTransfer {
	return &kernel.RailTransfer{ID: id, Kind: kernel.RailKindDeposit, Party: from,
		Amount: amount, Credit: amount, Status: kernel.RailStatusHeld, CreatedAt: time.Now().UTC()}
}

// Money in with a known owner is one commit: it crosses into the ledger and reaches that owner, so
// no state exists in which the credits are here but belong to nobody (D23).
func TestCreateRailDepositDeliversInOneCommit(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()

	e, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:tx-1", "0xalice", 300), alice)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	if e == nil || e.Amount != 300 {
		t.Fatalf("the delivery must be recorded: %+v", e)
	}
	if a, l := balances(t, db, alice); a != 300 || l != 0 {
		t.Errorf("owner: available=%d locked=%d, want 300/0", a, l)
	}
	if a, l := balances(t, db, sys); a != 0 || l != 0 {
		t.Errorf("operator keeps nothing of a fully attributed payment: %d/%d", a, l)
	}
	pos, err := db.RailPosition(ctx, sys)
	if err != nil {
		t.Fatal(err)
	}
	if pos.Vault != 300 || pos.Gap() != 0 {
		t.Errorf("vault=%d gap=%d, want 300 and 0", pos.Vault, pos.Gap())
	}
}

// Money from a sender nobody knows is held rather than given away, and stays out of the operator's
// reach until somebody claims it.
func TestHeldDepositIsNotSpendable(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()

	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:tx-1", "0xstranger", 300), ""); err != nil {
		t.Fatal(err)
	}
	if a, l := balances(t, db, sys); a != 0 || l != 300 {
		t.Fatalf("held money must not be spendable: available=%d locked=%d", a, l)
	}
	// Reconciliation leaves an unclaimed payment exactly where it is.
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 0 {
		t.Fatalf("an unclaimed payment was given away: %d %v", len(n), err)
	}
	// Once its sender registers, it is delivered, and the operator's hold ends.
	if err := db.SetRailAddress(ctx, alice, "0xstranger", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 1 {
		t.Fatalf("reconcile after registration: %d %v", len(n), err)
	}
	if a, _ := balances(t, db, alice); a != 300 {
		t.Errorf("owner after attribution: %d, want 300", a)
	}
	if a, l := balances(t, db, sys); a != 0 || l != 0 {
		t.Errorf("operator after attribution: %d/%d, want 0/0", a, l)
	}
	// Reconciling again moves nothing: the payment is spent.
	if n, _ := db.ReconcileDeposits(ctx, sys, 10); len(n) != 0 {
		t.Errorf("a delivered payment was delivered again (%d)", len(n))
	}
	if a, _ := balances(t, db, alice); a != 300 {
		t.Errorf("attributing twice paid twice: %d", a)
	}
}

// One fact, one movement — however many times it is presented, and whichever path found it.
func TestRailDepositIsKeyedByItsFact(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:tx-1", "0xalice", 300), alice); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if a, _ := balances(t, db, alice); a != 300 {
		t.Errorf("one payment booked %d times over: %d", 3, a)
	}
	// The same fact on other terms is a different claim about the world, and is refused rather
	// than silently preferred one way or the other.
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:tx-1", "0xalice", 999), alice); err == nil {
		t.Error("the same payment with another amount must be refused")
	}
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:tx-1", "0xsomeone", 300), alice); err == nil {
		t.Error("the same payment from another sender must be refused")
	}
}

// Money on its way out is nobody's to spend from the instant it is authorized, and crossing out is
// keyed by the transaction that carried it.
func TestReserveAndFinalizeAPayout(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:in", "0xalice", 500), alice); err != nil {
		t.Fatal(err)
	}

	id := uuid.NewString()
	row := &kernel.RailTransfer{ID: id, Kind: kernel.RailKindPayout, Party: alice, Amount: 200,
		Credit: 200, Destination: "0xalice", Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, row); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if a, _ := balances(t, db, alice); a != 300 {
		t.Errorf("owner after reserving: %d, want 300", a)
	}
	if a, l := balances(t, db, sys); a != 0 || l != 200 {
		t.Errorf("money in transit must be held, not spendable: %d/%d", a, l)
	}
	pos, _ := db.RailPosition(ctx, sys)
	if pos.PendingPayouts != 200 || pos.Gap() != 0 {
		t.Errorf("in transit=%d gap=%d", pos.PendingPayouts, pos.Gap())
	}

	if err := db.FinalizeRailTransfer(ctx, sys, id, "0xdeadbeef", time.Now().UTC()); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if a, l := balances(t, db, sys); a != 0 || l != 0 {
		t.Errorf("the hold must end when the payment is final: %d/%d", a, l)
	}
	pos, _ = db.RailPosition(ctx, sys)
	if pos.Vault != 300 || pos.Gap() != 0 {
		t.Errorf("after paying out: vault=%d gap=%d, want 300 and 0", pos.Vault, pos.Gap())
	}
	// Finalizing again changes nothing.
	if err := db.FinalizeRailTransfer(ctx, sys, id, "0xdeadbeef", time.Now().UTC()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	pos, _ = db.RailPosition(ctx, sys)
	if pos.Vault != 300 {
		t.Errorf("finalizing twice crossed out twice: vault=%d", pos.Vault)
	}
}

// A payment that finalized without moving money gives back exactly what it took, once, and the
// entries that recorded the attempt are left standing.
func TestCompensateReturnsExactlyWhatWasReserved(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:in", "0xalice", 500), alice); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	row := &kernel.RailTransfer{ID: id, Kind: kernel.RailKindPayout, Party: alice, Amount: 200,
		Credit: 200, Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, row); err != nil {
		t.Fatal(err)
	}
	before, err := db.ListLedgerByUser(ctx, alice, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.CompensateRailTransfer(ctx, sys, id, time.Now().UTC()); err != nil {
			t.Fatalf("compensate %d: %v", i, err)
		}
	}
	if a, _ := balances(t, db, alice); a != 500 {
		t.Errorf("owner after compensation: %d, want 500", a)
	}
	after, _ := db.ListLedgerByUser(ctx, alice, 100, 0)
	if len(after) != len(before)+1 {
		t.Errorf("compensation must add exactly one entry: %d → %d", len(before), len(after))
	}
	pos, _ := db.RailPosition(ctx, sys)
	if pos.Gap() != 0 {
		t.Errorf("gap after compensation: %d", pos.Gap())
	}
}

// Fuel comes out of the operator's own balance, never user backing: the authorized maximum is set
// aside, and only what was actually consumed is booked.
func TestRefillBooksTheCostNotTheAuthorization(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	// The operator has earnings of its own to spend on fuel.
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:earn", "0xop", 100), sys); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:in", "0xalice", 500), alice); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	row := &kernel.RailTransfer{ID: id, Kind: kernel.RailKindPayout, Party: alice, Amount: 10,
		Credit: 10, Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, row); err != nil {
		t.Fatal(err)
	}

	refill := &kernel.RailTransfer{ID: "refill-1", Kind: kernel.RailKindRefill, Amount: 40,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.RecordRailOutcome(ctx, sys, id, kernel.RailStatusRefilling, "", "", refill); err != nil {
		t.Fatalf("record refill: %v", err)
	}
	if a, l := balances(t, db, sys); a != 60 || l != 50 {
		t.Fatalf("the authorized maximum must leave the operator's own balance: %d/%d", a, l)
	}
	// Re-presenting the same outcome must not set the money aside twice.
	if err := db.RecordRailOutcome(ctx, sys, id, kernel.RailStatusRefilling, "", "", refill); err != nil {
		t.Fatal(err)
	}
	if a, _ := balances(t, db, sys); a != 60 {
		t.Errorf("one refill authorized twice: %d", a)
	}

	if err := db.BookRefill(ctx, sys, "refill-1", 25, true, time.Now().UTC()); err != nil {
		t.Fatalf("book: %v", err)
	}
	// Only the cost leaves; the rest of the authorization comes back.
	if a, l := balances(t, db, sys); a != 75 || l != 10 {
		t.Errorf("after booking: available=%d locked=%d, want 75 and 10", a, l)
	}
	pos, _ := db.RailPosition(ctx, sys)
	if pos.Gap() != 0 || pos.RefillLocks != 0 {
		t.Errorf("gap=%d locks=%d after booking", pos.Gap(), pos.RefillLocks)
	}
}

// The worker's question and the reader's are different: a finished payment is history somebody may
// want to see, but never work to redo.
func TestOpenAndAllRailTransfersAreDifferentQuestions(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:in", "0xalice", 500), alice); err != nil {
		t.Fatal(err)
	}
	done, open := uuid.NewString(), uuid.NewString()
	for _, id := range []string{done, open} {
		row := &kernel.RailTransfer{ID: id, Kind: kernel.RailKindPayout, Party: alice, Amount: 50,
			Credit: 50, Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
		if err := db.ReserveRailTransfer(ctx, sys, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinalizeRailTransfer(ctx, sys, done, "0xabc", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListRailTransfers(ctx, kernel.RailKindPayout, alice, "", 100, 0)
	if err != nil || len(all) != 2 {
		t.Errorf("the owner must see every withdrawal they made: %d %v", len(all), err)
	}
	pending, err := db.ListOpenRailTransfers(ctx, 100)
	if err != nil || len(pending) != 1 || pending[0].ID != open {
		t.Errorf("only unfinished work is the worker's: %d %v", len(pending), err)
	}
}

// One address belongs to one account, whoever registers it first, and it is found by exactly the
// canonical form that was stored.
func TestRailAddressIsUniqueAcrossAccounts(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	bob := newUser("bob", 0)
	if err := db.CreateUser(ctx, bob); err != nil {
		t.Fatal(err)
	}

	if err := db.SetRailAddress(ctx, alice, "0xabc", time.Now().UTC()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := db.SetRailAddress(ctx, bob.ID, "0xabc", time.Now().UTC()); err == nil {
		t.Error("one address must belong to one account")
	}
	// The address is what reconciliation attributes a payment by, so the registration must be what
	// a payment from that sender reaches — and a payment from an unregistered one must reach nobody.
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:known", "0xabc", 30), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:stranger", "0xnobody", 30), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReconcileDeposits(ctx, sys, 10); err != nil {
		t.Fatal(err)
	}
	if a, _ := balances(t, db, alice); a != 30 {
		t.Errorf("the registered sender's payment reached %d, want 30", a)
	}
	if a, _ := balances(t, db, bob.ID); a != 0 {
		t.Errorf("the account that lost the address was credited: %d", a)
	}
	if r, _ := db.ReadRailTransfer(ctx, "rail:stranger"); r.Status != kernel.RailStatusHeld {
		t.Errorf("an unregistered sender's payment must stay held: %s", r.Status)
	}
}

// A payment already held is what the operator's attribution names: booking it again with an owner
// delivers it in that commit, rather than answering with nothing.
func TestBookingAHeldPaymentWithAnOwnerDeliversIt(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:tx-1:0", "0xunknown", 300), ""); err != nil {
		t.Fatal(err)
	}
	e, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:tx-1:0", "0xunknown", 300), alice)
	if err != nil || e == nil || e.Amount != 300 {
		t.Fatalf("attributing a held payment: %+v %v", e, err)
	}
	if a, _ := balances(t, db, alice); a != 300 {
		t.Errorf("owner: %d, want 300", a)
	}
	if _, l := balances(t, db, sys); l != 0 {
		t.Errorf("the hold must end: %d", l)
	}
}

// The worker lists only what it drives. Held payments and announced claims wait on other people,
// and a flood of them must not push a withdrawal out of its sight.
func TestWorkerListIgnoresWhatItDoesNotDrive(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:in:0", "0xalice", 500), alice); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if _, err := db.CreateRailDeposit(ctx, sys, depositRow(uuid.NewString(), "0xdust", 1), ""); err != nil {
			t.Fatal(err)
		}
	}
	told := uuid.NewString()
	obligation := &kernel.RailTransfer{ID: told, Kind: kernel.RailKindObligation, Party: alice, Amount: 5, Credit: 5,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, obligation); err != nil {
		t.Fatal(err)
	}
	if err := db.FinalizeRailTransfer(ctx, sys, told, "0xtold", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRailOutcome(ctx, sys, told, kernel.RailStatusAnnounced, "0xtold", "", nil); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	row := &kernel.RailTransfer{ID: id, Kind: kernel.RailKindPayout, Party: alice, Amount: 50, Credit: 50,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, row); err != nil {
		t.Fatal(err)
	}
	open, err := db.ListOpenRailTransfers(ctx, 500)
	if err != nil || len(open) != 1 || open[0].ID != id {
		t.Fatalf("the worker must see exactly the payout: %d %v", len(open), err)
	}
}

// A crossing that has landed leaves the rail worker's list, whichever kind it is. An obligation
// payment is not finished at that point — the seller still has to be told — but telling it belongs
// to the reveal worker, which finds it by the call it paid for rather than by driving the row.
func TestTheRailWorkerIsDoneWhenTheCrossingLands(t *testing.T) {
	db, sys, alice := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:in:0", "0xalice", 500), alice); err != nil {
		t.Fatal(err)
	}
	payout, obligation := uuid.NewString(), uuid.NewString()
	for id, kind := range map[string]string{payout: kernel.RailKindPayout, obligation: kernel.RailKindObligation} {
		row := &kernel.RailTransfer{ID: id, Kind: kind, Party: alice, Amount: 50, Credit: 50,
			Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
		if err := db.ReserveRailTransfer(ctx, sys, row); err != nil {
			t.Fatal(err)
		}
		if err := db.FinalizeRailTransfer(ctx, sys, id, "0x"+id[:8], time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	open, err := db.ListOpenRailTransfers(ctx, 100)
	if err != nil || len(open) != 0 {
		t.Fatalf("a crossing that has landed is nothing more for the rail worker to do: %d %v", len(open), err)
	}
	_, _ = payout, obligation
}

// A fuel lock is taken before the rail signs, at everything the operator could spend, and settles
// once the purchase is known: bound to the purchase, sized to its maximum, the rest returned. Binding
// the same purchase again changes nothing; a lock that bought nothing is given back whole.
func TestRefillLockSettlesAtThePurchase(t *testing.T) {
	db, sys, _ := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:earn", "0xop", 100), sys); err != nil {
		t.Fatal(err)
	}
	pay := &kernel.RailTransfer{ID: "p1", Kind: kernel.RailKindPayout, Party: sys, Amount: 10, Credit: 10,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, pay); err != nil {
		t.Fatal(err)
	}
	lock := &kernel.RailTransfer{ID: "p1:fuel", Kind: kernel.RailKindRefill, Amount: 90,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.RecordRailOutcome(ctx, sys, pay.ID, kernel.RailStatusRefilling, "", "", lock); err != nil {
		t.Fatal(err)
	}
	if a, l := balances(t, db, sys); a != 0 || l != 100 {
		t.Fatalf("before signing everything is held: %d/%d", a, l)
	}
	if err := db.BindRefill(ctx, sys, lock.ID, "purchase-1", 40); err != nil {
		t.Fatal(err)
	}
	if a, l := balances(t, db, sys); a != 50 || l != 50 {
		t.Fatalf("after binding only the maximum is held: %d/%d", a, l)
	}
	if err := db.BindRefill(ctx, sys, lock.ID, "purchase-1", 40); err != nil {
		t.Fatal(err)
	}
	if a, l := balances(t, db, sys); a != 50 || l != 50 {
		t.Fatalf("binding again moved money: %d/%d", a, l)
	}
	if err := db.BindRefill(ctx, sys, lock.ID, "purchase-2", 40); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("a lock stands for one purchase: %v", err)
	}
	if got, _ := db.ReadRailTransferByRefill(ctx, "purchase-1"); got == nil || got.ID != lock.ID {
		t.Errorf("the lock must be found by its purchase: %+v", got)
	}
	if got, _ := db.ReadRailTransferByRefill(ctx, "purchase-9"); got != nil {
		t.Errorf("an unknown purchase is bound to nothing: %+v", got)
	}
	if err := db.ReleaseRefill(ctx, sys, lock.ID); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("a lock bound to a purchase cannot be given back: %v", err)
	}

	// A lock that bought nothing goes back whole and leaves no row behind.
	idle := &kernel.RailTransfer{ID: "p1:fuel2", Kind: kernel.RailKindRefill, Amount: 50,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.RecordRailOutcome(ctx, sys, pay.ID, kernel.RailStatusRefilling, "", "", idle); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseRefill(ctx, sys, idle.ID); err != nil {
		t.Fatal(err)
	}
	if a, l := balances(t, db, sys); a != 50 || l != 50 {
		t.Fatalf("a released lock must return exactly what it held: %d/%d", a, l)
	}
	if got, _ := db.ReadRailTransfer(ctx, idle.ID); got != nil {
		t.Errorf("a lock that bought nothing must leave no row: %+v", got)
	}
}

// A payment for a won draw is money in transit exactly like a withdrawal, and stops being so when
// the seller has been told: at that point the buyer's side of the trade is finished.
func TestAnObligationPaymentIsInTransitUntilTheSellerIsTold(t *testing.T) {
	db, sys, buyer := railFixture(t)
	ctx := context.Background()
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:earn", "0xbuyer", 100), buyer); err != nil {
		t.Fatal(err)
	}
	pay := &kernel.RailTransfer{ID: "won-1", Kind: kernel.RailKindObligation, Party: buyer,
		Destination: "0xseller", Amount: 40, Credit: 40,
		Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, pay); err != nil {
		t.Fatal(err)
	}
	// The money leaves the buyer and is held by the operator, which is what presents it to the rail.
	if a, _ := balances(t, db, buyer); a != 60 {
		t.Fatalf("the payment must come out of the buyer's balance: %d", a)
	}
	if _, l := balances(t, db, sys); l != 40 {
		t.Fatalf("the operator must hold what it is about to send: %d", l)
	}
	if pos, _ := db.RailPosition(ctx, sys); pos.PendingPayouts != 40 {
		t.Errorf("a reserved payment is money in transit: %+v", pos)
	}
	if err := db.FinalizeRailTransfer(ctx, sys, pay.ID, "0xhash", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRailOutcome(ctx, sys, pay.ID, kernel.RailStatusAnnounced, "0xhash", "", nil); err != nil {
		t.Fatal(err)
	}
	if pos, _ := db.RailPosition(ctx, sys); pos.PendingPayouts != 0 {
		t.Errorf("a payment the seller has heard of is finished, not in transit: %+v", pos)
	}
}

// owedFixture is a seller, a buyer's peer account, and the operator: everyone an obligation needs.
func owedFixture(t *testing.T) (*DB, string, *kernel.Account, *kernel.Account) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	sys, seller := newUser("sys", 0), newUser("seller", 100000)
	for _, u := range []*kernel.Account{sys, seller} {
		if err := db.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	peer := newPeer(t, db, "buyer-peer", "kbuyerAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 0, 0, time.Now().UTC())
	return db, sys.ID, seller, peer
}

// foreignCall is one inbound call as the store sees it: the peer's name for it, the trace that froze
// its terms, and the process that funds it.
type foreignCall struct {
	rec *kernel.IdempotencyRecord
	p   *kernel.Process
	tr  *kernel.Trace
}

// admit books an inbound foreign call exactly as the kernel does: the peer's record is opened, the
// credit limit is charged the most the call can owe, and the trace is written with that reserve in
// its frozen terms — all before any work is done.
func admit(t *testing.T, db *DB, seller, peer *kernel.Account, id string, dmax, limit int64) foreignCall {
	return admitFrom(t, db, seller, peer, id, "0xbuyer", dmax, limit)
}

// admitFrom is admit with the buyer's proven payer named: it is frozen on the trace, so the
// obligation knows where its money must come from before any of it can arrive.
func admitFrom(t *testing.T, db *DB, seller, peer *kernel.Account, id, payer string, dmax, limit int64) foreignCall {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	rec := &kernel.IdempotencyRecord{ID: uuid.NewString(), IdempotencyKey: id, CounterpartyUserID: peer.ID,
		Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := db.InsertPendingIdempotencyRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	terms := fmt.Sprintf(`{"reserve":%d,"nonce":"0a0b","commitment":"cm","lottery":1000}`, dmax)
	p := newProcess(seller.ID)
	tr := &kernel.Trace{ID: uuid.NewString(), ProcessID: p.ID, ActionOwnerID: seller.ID, ActionID: "a",
		CallerUserID: peer.ID, IdempotencyRecordID: &rec.ID, DispatchJSON: &terms, OwedRailAddress: payer, CreatedAt: now}
	if err := db.BeginRun(ctx, p, tr, seller.ID, dmax, dmax, limit); err != nil {
		t.Fatalf("admit %s: %v", id, err)
	}
	return foreignCall{rec: rec, p: p, tr: tr}
}

// settle commits the call through the real path, charging what it says: the receipt's charge is
// what the obligation is read off, and the commit is what corrects the exposure.
func settle(t *testing.T, db *DB, seller *kernel.Account, c foreignCall, charge int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	tx := &kernel.Transaction{ID: uuid.NewString(), ProcessID: c.p.ID, TraceID: c.tr.ID, OwnerUserID: seller.ID,
		CallerUserID: c.tr.CallerUserID, TargetUserID: seller.ID, ActionID: "a", Status: kernel.TxSuccess,
		Gross: charge, Net: charge, StartedAt: now, EndedAt: now}
	receipt := &kernel.Receipt{ID: uuid.NewString(), IssuerUserID: seller.ID, TxID: tx.ID, TraceID: c.tr.ID,
		ActionID: "a", Status: kernel.TxSuccess, Gross: charge, Net: charge, Charge: charge, Nonce: "0a0b", CreatedAt: now}
	// A settlement's taxable is what the row holds (D2), and the commit says so: what this call
	// charges less than its allocation is what settled children consumed, simulated here.
	if _, err := db.db.ExecContext(ctx, `UPDATE traces SET available=? WHERE id=?`, charge, c.tr.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitCall(ctx, tx, receipt, c.tr.ID, c.p.ID, kernel.CallerProcess, seller.ID, "", charge, 0, nil, c.rec.ID, ""); err != nil {
		t.Fatal(err)
	}
}

// paymentIn is a payment observed on the rail: the sender, the transaction it rode in, and what it
// carried. Nobody is credited yet — it is held until an obligation claims it.
func paymentIn(key, from, txHash string, amount int64) *kernel.RailTransfer {
	row := depositRow(key, from, amount)
	row.TxHash = txHash
	return row
}

// announce puts an obligation where the buyer has said it paid: settled, then revealed as a win.
// The payment is nil because this is a world with addresses — the money is observed on its own.
func announce(t *testing.T, db *DB, seller *kernel.Account, c foreignCall, txHash string, obligation, amount int64) {
	t.Helper()
	settle(t, db, seller, c, obligation)
	if err := db.ApplyReveal(context.Background(), "", c.tr.ID, amount, txHash, nil); err != nil {
		t.Fatal(err)
	}
}

// An obligation is readable from admission, not from commit, so a crash between the two leaves the
// exposure it reserved recoverable rather than reserved against nothing. The reserve is the most the
// call can owe; the commit corrects the counter to what it charged — and a call that charged nothing
// owes nothing and gives its whole reservation back, so a stream of free or fully-refunded calls
// cannot exhaust the limit.
func TestAnObligationIsReservedAtAdmissionAndCorrectedAtCommit(t *testing.T) {
	for _, charged := range []int64{45, 0} {
		t.Run(fmt.Sprintf("charged %d of a possible 60", charged), func(t *testing.T) {
			db, _, seller, peer := owedFixture(t)
			ctx := context.Background()
			call := admit(t, db, seller, peer, "call-1", 60, 1000)

			r, err := db.ReadOwed(ctx, "call-1", peer.ID)
			if err != nil || r == nil {
				t.Fatalf("no obligation after admission: %v", err)
			}
			if r.Settled || r.Obligation != 0 || r.Status != "" {
				t.Fatalf("at admission = %+v, want unsettled and owing nothing yet", r)
			}
			if e, _ := db.Exposure(ctx); e != 60 {
				t.Fatalf("exposure after admission = %d, want the 60 it could owe", e)
			}

			settle(t, db, seller, call, charged)
			got, _ := db.ReadOwed(ctx, "call-1", peer.ID)
			if !got.Settled || got.Obligation != charged {
				t.Errorf("after commit = %+v, want settled at %d", got, charged)
			}
			if e, _ := db.Exposure(ctx); e != charged {
				t.Errorf("exposure after charging %d = %d", charged, e)
			}
		})
	}
}

// Foreign work is admitted while the kernel's own unpaid delivered service stays inside its limit,
// and a second identity cannot use the headroom the first consumed: the counter belongs to the
// kernel, not to any peer, which is what makes minting identities pointless. A local caller is
// prepaid and never touches it.
func TestExposureAdmitsInsideTheLimit(t *testing.T) {
	db, _, seller, peerA := owedFixture(t)
	ctx := context.Background()
	peerB := newPeer(t, db, "peerB", "kbuyerBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", 0, 0, time.Now().UTC())
	const limit = 100

	try := func(peer *kernel.Account, price int64) error {
		p := newProcess(seller.ID)
		tr := &kernel.Trace{ID: uuid.NewString(), ProcessID: p.ID, CallerUserID: peer.ID, CreatedAt: time.Now().UTC()}
		return db.BeginRun(ctx, p, tr, seller.ID, price, price, limit)
	}

	if err := try(peerA, 60); err != nil {
		t.Fatalf("the first foreign call is inside the limit: %v", err)
	}
	if err := try(peerB, 60); !errors.Is(err, kernel.ErrInsufficientFunds) {
		t.Fatalf("a second identity must not double the limit, got %v", err)
	}
	if err := try(peerB, 40); err != nil {
		t.Fatalf("what is left of the limit is still available: %v", err)
	}
	if e, _ := db.Exposure(ctx); e != 100 {
		t.Errorf("exposure after admitting 60 and 40 = %d, want 100", e)
	}

	local := newUser("local", 500)
	if err := db.CreateUser(ctx, local); err != nil {
		t.Fatal(err)
	}
	p := newProcess(local.ID)
	tr := &kernel.Trace{ID: uuid.NewString(), ProcessID: p.ID, CallerUserID: local.ID, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, tr, local.ID, 200, 0, 0); err != nil {
		t.Fatalf("a local call is unaffected by the credit limit: %v", err)
	}
	if e, _ := db.Exposure(ctx); e != 100 {
		t.Errorf("a local call moved the exposure counter to %d", e)
	}
}

// The limit is enforced by the write itself rather than by a read before it, so calls arriving
// together cannot both pass a limit that admits only one. This is the only way the limit can be
// breached in production: several admissions at once, each reading a counter true a moment ago.
func TestExposureAdmissionIsAtomicUnderConcurrency(t *testing.T) {
	db, _, seller, peer := owedFixture(t)
	ctx := context.Background()
	const limit, price, n = int64(100), int64(30), 16

	var wg sync.WaitGroup
	admitted := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := newProcess(seller.ID)
			tr := &kernel.Trace{ID: uuid.NewString(), ProcessID: p.ID, CallerUserID: peer.ID, CreatedAt: time.Now().UTC()}
			admitted[i] = db.BeginRun(ctx, p, tr, seller.ID, price, price, limit) == nil
		}(i)
	}
	wg.Wait()

	got, err := db.Exposure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got > limit {
		t.Errorf("concurrent admission carried exposure to %d, past the limit of %d", got, limit)
	}
	count := 0
	for _, ok := range admitted {
		if ok {
			count++
		}
	}
	if want := int(limit / price); count != want {
		t.Errorf("admitted %d concurrent calls of %d against a limit of %d, want %d", count, price, limit, want)
	}
}

// A payment closes an obligation only if it is the one the buyer named, from the sender it proved,
// for the amount the draw decided; and it closes at most one, because crediting it spends it. The
// seller is credited the WHOLE payment: the draw pays the face value or nothing and its expected
// value is the obligation, so a kernel keeping the difference would take a position in its users'
// trades and underpay every seller that wins.
func TestOnlyTheNamedPaymentClosesAnObligationAndItCreditsTheWholeOfIt(t *testing.T) {
	db, sys, seller, peer := owedFixture(t)
	ctx := context.Background()
	first := admit(t, db, seller, peer, "call-1", 11, 100000)
	announce(t, db, seller, first, "0xpaid", 11, 1000)
	// A second obligation naming the very same payment, waiting alongside the first.
	second := admit(t, db, seller, peer, "call-2", 11, 100000)
	announce(t, db, seller, second, "0xpaid", 11, 1000)

	for _, wrong := range []struct {
		why               string
		key, from, txHash string
		amount            int64
	}{
		{"a stranger's payment in the named transaction", "rail:a", "0xstranger", "0xpaid", 1000},
		{"the buyer's payment in another transaction", "rail:b", "0xbuyer", "0xother", 1000},
		{"the buyer's payment for another amount", "rail:c", "0xbuyer", "0xpaid", 999},
	} {
		if _, err := db.CreateRailDeposit(ctx, sys, paymentIn(wrong.key, wrong.from, wrong.txHash, wrong.amount), ""); err != nil {
			t.Fatal(err)
		}
		if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 0 {
			t.Errorf("%s closed the obligation (%d, %v)", wrong.why, len(n), err)
		}
	}

	before, _ := balances(t, db, seller.ID)
	if _, err := db.CreateRailDeposit(ctx, sys, paymentIn("rail:right", "0xbuyer", "0xpaid", 1000), ""); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 1 {
		t.Fatalf("the buyer's own named payment must close it: %d %v", len(n), err)
	}
	if after, _ := balances(t, db, seller.ID); after-before != 1000 {
		t.Errorf("the seller was credited %d for a face value of 1000 on an obligation of 11", after-before)
	}
	if r, _ := db.ReadOwed(ctx, "call-1", peer.ID); r.Status != kernel.OwedCredited {
		t.Errorf("a paid obligation must close: %s", r.Status)
	}
	// Only cash reduces exposure, and it reduces it by the cash: 22 delivered across the two
	// obligations, 1000 received for the one that was paid.
	if e, _ := db.Exposure(ctx); e != 22-1000 {
		t.Errorf("exposure after the payment = %d, want %d", e, 22-1000)
	}
	// The payment was spent on the first, so the second is still waiting, and reconciling again
	// moves nothing at all.
	if r, _ := db.ReadOwed(ctx, "call-2", peer.ID); r.Status != kernel.OwedAnnounced {
		t.Errorf("one payment closed two obligations: the second is %s", r.Status)
	}
	held, _ := balances(t, db, seller.ID)
	if n, _ := db.ReconcileDeposits(ctx, sys, 10); len(n) != 0 {
		t.Errorf("a spent payment closed another obligation (%d)", len(n))
	}
	if after, _ := balances(t, db, seller.ID); after != held {
		t.Errorf("a spent payment paid again: %d → %d", held, after)
	}
}

// A losing draw closes the obligation and moves nothing, and the exposure it created stays: only
// cash reduces exposure, which is what stops a buyer taking delivery for free at scale.
func TestALosingDrawLeavesTheExposureBehind(t *testing.T) {
	db, sys, seller, peer := owedFixture(t)
	ctx := context.Background()
	call := admit(t, db, seller, peer, "call-1", 40, 100000)
	settle(t, db, seller, call, 40)
	before, _ := balances(t, db, seller.ID)
	if err := db.ApplyReveal(ctx, sys, call.tr.ID, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if r, _ := db.ReadOwed(ctx, "call-1", peer.ID); r.Status != kernel.OwedCancelled || r.Amount != 0 {
		t.Errorf("a losing draw = %+v, want cancelled at 0", r)
	}
	if after, _ := balances(t, db, seller.ID); after != before {
		t.Errorf("a losing draw moved money: %d → %d", before, after)
	}
	if e, _ := db.Exposure(ctx); e != 40 {
		t.Errorf("exposure after a losing draw = %d, want the 40 that was delivered", e)
	}
}

// Where the world has no addresses the reveal is itself the payment (D23), so the two are one
// commit: the money is booked with the fact that made it final, reconciliation then closes the
// obligation and credits the seller, and none of it needs an operator.
func TestARevealBooksItsOwnPaymentWhereThereAreNoAddresses(t *testing.T) {
	db, sys, seller, peer := owedFixture(t)
	ctx := context.Background()
	call := admitFrom(t, db, seller, peer, "call-1", "", 40, 100000)
	settle(t, db, seller, call, 40)
	before, _ := balances(t, db, seller.ID)

	pay := depositRow("rail:ref:manual:call-1", "", 40)
	pay.TxHash = "manual:call-1"
	if err := db.ApplyReveal(ctx, sys, call.tr.ID, 40, pay.TxHash, pay); err != nil {
		t.Fatal(err)
	}
	if row, _ := db.ReadRailTransfer(ctx, pay.ID); row == nil || row.Status != kernel.RailStatusHeld {
		t.Fatalf("the reveal booked no held payment: %+v", row)
	}
	// A resend is answered from the record, so it must book nothing a second time: the guard that
	// makes the reveal idempotent is the same one the payment is written under.
	if err := db.ApplyReveal(ctx, sys, call.tr.ID, 40, pay.TxHash, pay); err != nil {
		t.Fatalf("a resent reveal must be safe: %v", err)
	}
	if _, err := db.ReconcileDeposits(ctx, sys, 10); err != nil {
		t.Fatal(err)
	}
	if r, _ := db.ReadOwed(ctx, "call-1", peer.ID); r.Status != kernel.OwedCredited {
		t.Errorf("the obligation = %s, want credited with no operator act", r.Status)
	}
	if after, _ := balances(t, db, seller.ID); after != before+40 {
		t.Errorf("the seller was credited %d, want the whole 40 once", after-before)
	}
	if e, _ := db.Exposure(ctx); e != 0 {
		t.Errorf("exposure after the payment = %d, want 0: cash is what discharges it", e)
	}
}

// A reveal whose payment cannot be booked leaves no trace of itself either: the buyer will resend,
// and a reveal recorded without its money would be an obligation nothing could ever close.
func TestARevealThatCannotBookItsPaymentRecordsNothing(t *testing.T) {
	db, _, seller, peer := owedFixture(t)
	ctx := context.Background()
	call := admitFrom(t, db, seller, peer, "call-1", "", 40, 100000)
	settle(t, db, seller, call, 40)

	// A payment naming an account that does not exist: the crossing's own ledger entry fails.
	if err := db.ApplyReveal(ctx, "nobody", call.tr.ID, 40, "manual:call-1",
		depositRow("rail:ref:manual:call-1", "", 40)); err == nil {
		t.Fatal("a payment that could not be booked was accepted")
	}
	if r, _ := db.ReadOwed(ctx, "call-1", peer.ID); r.Status != "" {
		t.Errorf("the reveal survived its payment failing: %s", r.Status)
	}
}

// The operator sees every obligation this kernel is waiting on, not only the ones whose buyer has
// spoken: what bounds a buyer that goes quiet is the credit limit, and what acts on one is the
// operator, so an obligation nobody has revealed is exactly what must be visible.
func TestOpenObligationsAreListedBeforeTheirBuyerSpeaks(t *testing.T) {
	db, sys, seller, peer := owedFixture(t)
	ctx := context.Background()
	quiet := admit(t, db, seller, peer, "call-quiet", 40, 100000)
	settle(t, db, seller, quiet, 40)
	spoken := admit(t, db, seller, peer, "call-spoken", 40, 100000)
	announce(t, db, seller, spoken, "0xpaid", 40, 40)
	lost := admit(t, db, seller, peer, "call-lost", 40, 100000)
	settle(t, db, seller, lost, 40)
	if err := db.ApplyReveal(ctx, sys, lost.tr.ID, 0, "", nil); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ListOwed(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.ID] = r.Status
	}
	if s, ok := got["call-quiet"]; !ok || s != "" {
		t.Errorf("an unrevealed obligation = %q/%v, want listed with no status", s, ok)
	}
	if got["call-spoken"] != kernel.OwedAnnounced {
		t.Errorf("a revealed obligation = %q, want announced", got["call-spoken"])
	}
	if _, ok := got["call-lost"]; ok {
		t.Error("a losing draw owes nothing and must not be listed")
	}
}

// Only calls that owe something and can be told now are offered. A call that owes nothing left the
// seller no obligation, so revealing it could only fail forever and, being oldest, would starve
// every real reveal behind it; a payment still in flight would sit at the head of the queue for the
// same reason. Being told is what a payment's own row was still open for.
func TestOnlyRevealableDrawsAreOffered(t *testing.T) {
	db, sys, buyer := railFixture(t)
	ctx := context.Background()
	peer := newPeer(t, db, "seller-peer", "ksellerAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 0, 0, time.Now().UTC())
	if _, err := db.CreateRailDeposit(ctx, sys, depositRow("rail:in:0", "0xbuyer", 500), buyer); err != nil {
		t.Fatal(err)
	}
	lost := boughtCall(t, db, buyer, peer.ID, "lost", 11)
	free := boughtCall(t, db, buyer, peer.ID, "free", 0)
	inFlight := boughtCall(t, db, buyer, peer.ID, "in-flight", 11)

	pay := &kernel.RailTransfer{ID: "in-flight", Kind: kernel.RailKindObligation, Party: buyer,
		Destination: "0xseller", Amount: 50, Credit: 50, Status: kernel.RailStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.ReserveRailTransfer(ctx, sys, pay); err != nil {
		t.Fatal(err)
	}

	ids := pendingIDs(t, db)
	if !ids[lost] || ids[free] || ids[inFlight] {
		t.Fatalf("offered %v: only a settled draw that owes something and can be told now qualifies", ids)
	}
	if err := db.FinalizeRailTransfer(ctx, sys, "in-flight", "0xhash", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if ids := pendingIDs(t, db); !ids[inFlight] {
		t.Error("a payment that has landed must be offered for reveal")
	}
	if err := db.MarkRevealed(ctx, inFlight); err != nil {
		t.Fatal(err)
	}
	if ids := pendingIDs(t, db); ids[inFlight] {
		t.Error("an acknowledged draw was offered again")
	}
	if r, _ := db.ReadRailTransfer(ctx, "in-flight"); r.Status != kernel.RailStatusAnnounced {
		t.Errorf("the payment's own row must close when the seller has heard: %s", r.Status)
	}
}

// boughtCall stages the buyer's whole memory of a settled remote call: the trace holding the secret
// it committed to, and the transaction naming the peer it bought from and what it owes.
func boughtCall(t *testing.T, db *DB, buyerID, peerID, key string, owed int64) string {
	t.Helper()
	ctx := context.Background()
	dispatchJSON := `{"secret":"aabb","lottery":100}`
	p := newProcess(buyerID)
	tr := &kernel.Trace{ID: uuid.NewString(), ProcessID: p.ID, CallerUserID: buyerID,
		IdempotencyKey: &key, DispatchJSON: &dispatchJSON, CreatedAt: time.Now().UTC()}
	if err := db.BeginRun(ctx, p, tr, buyerID, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	now := timeToStr(time.Now().UTC())
	if err := db.ExecForTest(ctx,
		`INSERT INTO transactions (id,process_id,trace_id,parent_trace_id,owner_user_id,caller_user_id,
		   target_user_id,action_id,status,gross,net,started_at,ended_at)
		 VALUES (?,?,?,'',?,?,?,'a','success',11,?,?,?)`,
		uuid.NewString(), p.ID, tr.ID, buyerID, buyerID, peerID, owed, now, now); err != nil {
		t.Fatal(err)
	}
	return tr.ID
}

func pendingIDs(t *testing.T, db *DB) map[string]bool {
	t.Helper()
	rows, err := db.ListPendingReveals(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.TraceID] = true
	}
	return out
}

// preTicketDB builds a database one migration short of the per-call settlement upgrade, seeds the
// old-world state a test wants to strand, and returns its path for Open to try to upgrade.
func preTicketDB(t *testing.T, seed func(*sql.DB)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pre049.db")
	raw := rawDB(t, path)
	if _, err := raw.Exec(createSchemaMigrations); err != nil {
		t.Fatal(err)
	}
	files, err := migrationFileNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		version := strings.TrimSuffix(pathpkg.Base(file), ".sql")
		if version >= "049" {
			break
		}
		body, err := migrationFS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range splitSQLStatements(string(body)) {
			if _, err := raw.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", version, err)
			}
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, timeToStr(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
	now := timeToStr(time.Now().UTC())
	if _, err := raw.Exec(`INSERT INTO "accounts" (id,handle,available,locked,created_at,updated_at)
		VALUES ('u1','alice',0,0,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO kernels (public_key,first_seen,updated_at) VALUES ('peerkey',?,?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	seed(raw)
	raw.Close()
	return path
}

// The upgrade drops the records the old economy's money sat on, so it refuses to run while any of
// that money is still in flight: the kernel will not start until the operator drains it. Each shape
// below is value that would have no settlement path afterwards, and a database holding none of them
// upgrades — with its finished history marked already acknowledged, since those calls drew for
// nothing and carry no secret to reveal.
func TestTicketMigrationRefusesToStrandOldWorldValue(t *testing.T) {
	now := timeToStr(time.Now().UTC())
	peerAccount := func(raw *sql.DB, available, locked int64) {
		if _, err := raw.Exec(`INSERT INTO "accounts" (id,kernel_public_key,available,locked,created_at,updated_at)
			VALUES ('peer1','peerkey',?,?,?,?)`, available, locked, now, now); err != nil {
			t.Fatal(err)
		}
	}
	settlement := func(id, status string) func(*sql.DB) {
		return func(raw *sql.DB) {
			if _, err := raw.Exec(`INSERT INTO rail_transfers (id,kind,party,amount,credit,status,created_at)
				VALUES (?,'settlement','peer1',100,30,?,?)`, id, status, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	unsettledTrace := func(id, extraCol, extraVal string) func(*sql.DB) {
		return func(raw *sql.DB) {
			if _, err := raw.Exec(`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at)
				VALUES (?,'u1',0,0,'open',?)`, "p"+id, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO "traces" (id,process_id,caller_user_id,available,locked,`+extraCol+`,created_at)
				VALUES (?,?,'u1',100,0,`+extraVal+`,?)`, id, "p"+id, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		seed    func(*sql.DB)
		wantErr bool
	}{
		{"a peer holding a prepaid credit", func(raw *sql.DB) { peerAccount(raw, 500, 0) }, true},
		{"a peer holding a debt", func(raw *sql.DB) { peerAccount(raw, -500, 0) }, true},
		{"a peer with money locked", func(raw *sql.DB) { peerAccount(raw, 0, 500) }, true},
		{"an outbound call awaiting its receipt", unsettledTrace("t1", "idempotency_key", "'idem-1'"), true},
		{"an inbound call holding a serving markup", unsettledTrace("t2", "premium_parked", "5"), true},
		{"a settlement still being drawn for", settlement("sid1", "drawing"), true},
		{"a settlement paid and not yet through", settlement("sid2", "submitted"), true},
		{"nothing in flight", func(raw *sql.DB) {
			peerAccount(raw, 0, 0)
			settlement("sid3", "announced")(raw) // finished history passes
			// A cross-kernel call that finished before the upgrade: settled, owing something, made
			// to a peer. It has no secret and left that peer no obligation, so it must never be
			// queued for a reveal — one such row at the head of the queue starves every real one.
			if _, err := raw.Exec(`INSERT INTO processes (id,owner_user_id,available,locked,status,created_at)
				VALUES ('pold','u1',0,0,'closed',?)`, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO "traces" (id,process_id,caller_user_id,available,locked,idempotency_key,created_at)
				VALUES ('told','pold','u1',0,0,'idem-old',?)`, now); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO transactions (id,process_id,trace_id,parent_trace_id,owner_user_id,
				caller_user_id,target_user_id,action_id,status,gross,net,started_at,ended_at)
				VALUES ('xold','pold','told','','u1','u1','peer1','a','success',11,11,?,?)`, now, now); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(preTicketDB(t, tc.seed))
			if db != nil {
				defer db.Close()
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("the upgrade ran while old-world money was still in flight")
				}
				return
			}
			if err != nil {
				t.Fatalf("the upgrade refused a drained database: %v", err)
			}
			// History is acknowledged, so no pre-upgrade call is ever queued for a reveal it has no
			// secret for — one such row at the head of the queue would starve every real one.
			if n, err := db.ListPendingReveals(context.Background(), 10); err != nil || len(n) != 0 {
				t.Errorf("the upgrade queued %d historical calls for a reveal: %v", len(n), err)
			}
		})
	}
}

// A peer's payment can arrive from an address a local account also registered — plausible when one
// person runs both — so obligations are settled before any payment is attributed to a local sender.
// Crediting the local user first would hand it the seller's money, leave the seller unpaid, and
// leave the exposure standing with nothing that could ever reduce it.
func TestAnObligationIsSettledBeforeItsSenderIsAttributed(t *testing.T) {
	db, sys, seller, peer := owedFixture(t)
	ctx := context.Background()
	// A local user who has registered the very address the buyer's kernel pays from.
	collider := newUser("collider", 0)
	if err := db.CreateUser(ctx, collider); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRailAddress(ctx, collider.ID, "0xbuyer", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	call := admit(t, db, seller, peer, "call-1", 40, 100000)
	announce(t, db, seller, call, "0xpaid", 40, 40)

	if _, err := db.CreateRailDeposit(ctx, sys, paymentIn("rail:p", "0xbuyer", "0xpaid", 40), ""); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 1 {
		t.Fatalf("reconcile wrote %d credits: %v", len(n), err)
	}
	if a, _ := balances(t, db, seller.ID); a == 0 {
		t.Error("the seller was not paid: its money went to the local account sharing the address")
	}
	if a, _ := balances(t, db, collider.ID); a != 0 {
		t.Errorf("a local account was credited the seller's payment: %d", a)
	}
	if r, _ := db.ReadOwed(ctx, "call-1", peer.ID); r.Status != kernel.OwedCredited {
		t.Errorf("the obligation must close: %s", r.Status)
	}

	// A payment no obligation claims is that sender's own, and reaches it on the same pass.
	if _, err := db.CreateRailDeposit(ctx, sys, paymentIn("rail:q", "0xbuyer", "0xelse", 25), ""); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 1 {
		t.Fatalf("an unclaimed payment must reach its sender: %d %v", len(n), err)
	}
	if a, _ := balances(t, db, collider.ID); a != 25 {
		t.Errorf("the sender has %d, want the 25 nobody claimed", a)
	}
}

// The other ordering of the same collision: the buyer's winning payment can land, and be scanned,
// before its reveal arrives. No obligation claims it yet, and a local account has registered the
// paying address — so the payment must stay held rather than be given to that account, or the
// reveal that follows finds nothing to close and the seller is never paid. A losing reveal frees it.
func TestAPaymentFromAPeerWaitsForItsRevealBeforeAnyoneElseGetsIt(t *testing.T) {
	db, sys, seller, peer := owedFixture(t)
	ctx := context.Background()
	// The seller has never pulled this buyer's gossip: its only knowledge of the payer is what the
	// call itself proved and froze.
	collider := newUser("collider", 0)
	if err := db.CreateUser(ctx, collider); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRailAddress(ctx, collider.ID, "0xbuyer", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Settled, owing 40, and not yet revealed.
	call := admit(t, db, seller, peer, "call-1", 40, 100000)
	settle(t, db, seller, call, 40)

	if _, err := db.CreateRailDeposit(ctx, sys, paymentIn("rail:early", "0xbuyer", "0xpaid", 40), ""); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 0 {
		t.Fatalf("a payment that may be the seller's was given away before the reveal: %d %v", len(n), err)
	}
	if a, _ := balances(t, db, collider.ID); a != 0 {
		t.Fatalf("the local account was credited the peer's payment: %d", a)
	}

	// The reveal arrives: the payment is the obligation's, and closes it.
	if err := db.ApplyReveal(ctx, sys, call.tr.ID, 40, "0xpaid", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 1 {
		t.Fatalf("the revealed obligation was not closed by the waiting payment: %d %v", len(n), err)
	}
	if a, _ := balances(t, db, seller.ID); a == 0 {
		t.Error("the seller was not paid")
	}
	if r, _ := db.ReadOwed(ctx, "call-1", peer.ID); r.Status != kernel.OwedCredited {
		t.Errorf("the obligation must close: %s", r.Status)
	}

	// With nothing of the peer's unresolved, a later payment from that address is the local
	// account's own.
	if _, err := db.CreateRailDeposit(ctx, sys, paymentIn("rail:later", "0xbuyer", "0xelse", 25), ""); err != nil {
		t.Fatal(err)
	}
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 1 {
		t.Fatalf("an unclaimed payment must reach its registered sender: %d %v", len(n), err)
	}
	if a, _ := balances(t, db, collider.ID); a != 25 {
		t.Errorf("the local account has %d, want the 25 nobody could claim", a)
	}
}

// One obligation takes one payment, even when two payments match it — a transaction can carry two
// transfers of the same amount from the same sender — and nobody is handed a payment by name while
// an unresolved obligation may still be waiting for it, since that would put it beyond the reveal's
// reach for good. A payment no obligation can claim is the operator's to attribute.
func TestOneObligationTakesOnePaymentAndReservedMoneyIsNotHandedOut(t *testing.T) {
	db, sys, seller, peer := owedFixture(t)
	ctx := context.Background()
	collider := newUser("collider", 0)
	if err := db.CreateUser(ctx, collider); err != nil {
		t.Fatal(err)
	}
	call := admit(t, db, seller, peer, "call-1", 40, 100000)
	settle(t, db, seller, call, 40) // settled, unrevealed: the payer is known, the outcome is not

	twin := paymentIn("rail:0xpaid:1", "0xbuyer", "0xpaid", 40)
	if _, err := db.CreateRailDeposit(ctx, sys, twin, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRailDeposit(ctx, sys, twin, collider.ID); !errors.Is(err, kernel.ErrInvalidState) {
		t.Fatalf("a payment an unresolved obligation may be waiting for was handed to a local user: %v", err)
	}
	if _, err := db.CreateRailDeposit(ctx, sys, paymentIn("rail:0xpaid:0", "0xbuyer", "0xpaid", 40), ""); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyReveal(ctx, sys, call.tr.ID, 40, "0xpaid", nil); err != nil {
		t.Fatal(err)
	}
	before, _ := db.Exposure(ctx)
	if n, err := db.ReconcileDeposits(ctx, sys, 10); err != nil || len(n) != 1 {
		t.Fatalf("two matching payments closed the obligation %d times: %v", len(n), err)
	}
	if e, _ := db.Exposure(ctx); e != before-40 {
		t.Errorf("exposure fell by %d for one obligation of 40", before-e)
	}
	if a, _ := balances(t, db, seller.ID); a != 100000+40 {
		t.Errorf("the seller was credited %d, want 40 once", a-100000)
	}
	// The obligation is resolved, so whichever twin is still held is nobody's reserved money any
	// more, and the operator may attribute it.
	for _, key := range []string{"rail:0xpaid:0", "rail:0xpaid:1"} {
		if row, _ := db.ReadRailTransfer(ctx, key); row.Status == kernel.RailStatusHeld {
			if _, err := db.CreateRailDeposit(ctx, sys, row, collider.ID); err != nil {
				t.Fatalf("an unclaimed payment must be attributable once nothing is waiting: %v", err)
			}
		}
	}
	if a, _ := balances(t, db, collider.ID); a != 40 {
		t.Errorf("the local user has %d, want the unclaimed 40", a)
	}
}
