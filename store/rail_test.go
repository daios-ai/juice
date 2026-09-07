package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/daios-ai/juice/kernel"
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

	all, err := db.ListRailTransfers(ctx, kernel.RailKindPayout, alice, "", 100)
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
