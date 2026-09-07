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

	"github.com/google/uuid"

	"github.com/daios-ai/juice/kernel"
)

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
func announce(t *testing.T, db *DB, seller *kernel.Account, c foreignCall, txHash string, obligation, amount int64) {
	t.Helper()
	settle(t, db, seller, c, obligation)
	if err := db.ApplyReveal(context.Background(), c.tr.ID, amount, txHash); err != nil {
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
	db, _, seller, peer := owedFixture(t)
	ctx := context.Background()
	call := admit(t, db, seller, peer, "call-1", 40, 100000)
	settle(t, db, seller, call, 40)
	before, _ := balances(t, db, seller.ID)
	if err := db.ApplyReveal(ctx, call.tr.ID, 0, ""); err != nil {
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
	if err := db.ApplyReveal(ctx, call.tr.ID, 40, "0xpaid"); err != nil {
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
	if err := db.ApplyReveal(ctx, call.tr.ID, 40, "0xpaid"); err != nil {
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
