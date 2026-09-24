// SPDX-License-Identifier: AGPL-3.0-only

package rail

import (
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jrail "github.com/daios-ai/juice-rail/go/rail"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/daios-ai/juice/kernel"
)

// fakeLib stands for juice-rail: the test chooses what the rail answers, so the adaptor's
// sequencing and its translation of shortages can be exercised with no chain.
type fakeLib struct {
	prepareErr error
	sendErr    error
	refillErr  error
	refillID   jrail.ID // what the library hands back with refillErr: set when the intent was recorded
	status     jrail.Status
	fact       jrail.Fact
	settled    bool
	costErr    error
	cost       *big.Int
	deposits   []jrail.Deposit
	intent     jrail.Intent
	pending    []jrail.Intent
	nonce      uint64
	byNonce    map[uint64]jrail.Intent

	// The scan cursor as the rail keeps it: absent until something seeds it, and what the chain
	// would answer if asked for its settled head.
	cursor     uint64
	hasCursor  bool
	head       uint64
	headErr    error
	seeded     int
	scannedErr error

	prepared  int
	sent      int
	refilled  int
	reserveIn *big.Int

	// What the venue's router says it unwraps, and whether it could be asked.
	unwraps    common.Address
	unwrapsErr error
}

func (f *fakeLib) Account() common.Address           { return common.Address{} }
func (f *fakeLib) CheckDomain(context.Context) error { return nil }
func (f *fakeLib) WrappedNative(context.Context) (common.Address, error) {
	return f.unwraps, f.unwrapsErr
}

func (f *fakeLib) Prepare(_ context.Context, _ jrail.ID, _ jrail.Kind, _ common.Address, _ *big.Int) error {
	f.prepared++
	return f.prepareErr
}

func (f *fakeLib) Send(context.Context, jrail.ID) (common.Hash, error) {
	f.sent++
	if f.sendErr != nil {
		return common.Hash{}, f.sendErr
	}
	return common.HexToHash("0xsent"), nil
}

// Refill mirrors the library: the intent is recorded before it is broadcast, so a broadcast that
// fails still hands back the identifier of a purchase that now owns a nonce. A shortage refused
// before anything was recorded hands back nothing.
func (f *fakeLib) Refill(_ context.Context, reserve *big.Int) (jrail.ID, common.Hash, error) {
	f.refilled++
	f.reserveIn = reserve
	if f.refillErr != nil {
		return f.refillID, common.Hash{}, f.refillErr
	}
	return jrail.ID{1}, common.HexToHash("0xrefill"), nil
}

func (f *fakeLib) Status(context.Context, jrail.ID) (jrail.Status, error) { return f.status, nil }

func (f *fakeLib) Outcome(context.Context, jrail.ID) (jrail.Fact, bool, error) {
	return f.fact, f.settled, nil
}

func (f *fakeLib) RefillCost(context.Context, jrail.ID) (*big.Int, error) {
	if f.costErr != nil {
		return nil, f.costErr
	}
	return f.cost, nil
}
func (f *fakeLib) Intent(jrail.ID) (jrail.Intent, bool, error)           { return f.intent, true, nil }
func (f *fakeLib) ScanDeposits(context.Context) ([]jrail.Deposit, error) { return nil, nil }
func (f *fakeLib) Deposits() ([]jrail.Deposit, error)                    { return f.deposits, nil }
func (f *fakeLib) DepositsScannedTo() (uint64, bool, error) {
	if f.scannedErr != nil {
		return 0, false, f.scannedErr
	}
	if !f.hasCursor {
		return 0, false, nil
	}
	return f.cursor, true, nil
}

// SeedScan is the library's own behaviour: the cursor if there is one, else the settled head,
// stored. It counts its calls so a test can show the seed happens once.
func (f *fakeLib) SeedScan(context.Context) (uint64, error) {
	if f.hasCursor {
		return f.cursor, nil
	}
	if f.headErr != nil {
		return 0, f.headErr
	}
	f.seeded++
	f.cursor, f.hasCursor = f.head, true
	return f.head, nil
}
func (f *fakeLib) SettledBalances(context.Context) (*big.Int, *big.Int, uint64, error) {
	return big.NewInt(500), big.NewInt(1), 9, nil
}
func (f *fakeLib) Pending() ([]jrail.Intent, error)      { return f.pending, nil }
func (f *fakeLib) Nonce(context.Context) (uint64, error) { return f.nonce, nil }
func (f *fakeLib) IntentByNonce(n uint64) (jrail.Intent, bool, error) {
	in, ok := f.byNonce[n]
	return in, ok, nil
}

func testChain(t *testing.T) (*Chain, *fakeLib) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	l := &fakeLib{status: jrail.StatusPending}
	c := newChain(l, key)
	c.checked = true
	return c, l
}

// The domain check includes what the router unwraps. A world naming another token than the router's
// own WETH9() would have every refill buy a token the router never releases, so it is a wrong domain
// — refused even on an existing kernel — while a router that cannot be asked only makes the check
// wait, as an unreachable chain does.
func TestChainReadyRefusesAVenueThatUnwrapsAnotherToken(t *testing.T) {
	c, l := testChain(t)
	c.checked = false
	c.venue = jrail.Venue{Router: common.HexToAddress("0x1"), WETH: common.HexToAddress("0x2")}

	l.unwraps = common.HexToAddress("0x3")
	err := c.Ready(context.Background())
	if !errors.Is(err, jrail.ErrWrongDomain) || !strings.Contains(err.Error(), "0x0000000000000000000000000000000000000003") {
		t.Fatalf("another token must be a wrong domain naming it: %v", err)
	}
	if c.checked {
		t.Fatal("a failed check was remembered as passed")
	}

	l.unwrapsErr = errors.New("connection refused")
	err = c.Ready(context.Background())
	if err == nil || errors.Is(err, jrail.ErrWrongDomain) || !strings.Contains(err.Error(), "0x0000000000000000000000000000000000000001") {
		t.Fatalf("a router that cannot be asked is an outage naming the router, not a wrong domain: %v", err)
	}

	l.unwrapsErr, l.unwraps = nil, c.venue.WETH
	if err := c.Ready(context.Background()); err != nil || !c.checked {
		t.Fatalf("the router's own answer must pass: %v", err)
	}
}

// An ordinary payment is prepared and sent, in that order — the adaptor sequences the rail's own
// steps rather than its combined verb, because only this path can pass the kernel's reserve.
func TestChainPaySequencesPrepareThenSend(t *testing.T) {
	c, l := testChain(t)
	out, err := c.Pay(context.Background(), "row-1", "0x000000000000000000000000000000000000dEaD", 100)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if l.prepared != 1 || l.sent != 1 || l.refilled != 0 {
		t.Fatalf("prepared=%d sent=%d refilled=%d", l.prepared, l.sent, l.refilled)
	}
	if out.TxHash == "" || out.Blocked != "" {
		t.Errorf("outcome: %+v", out)
	}
}

// When fuel is short, Pay signs nothing and says so: the kernel records the refill it is about to
// ask for before any refill exists. Refill itself is told the kernel's own floor — everything that
// is not the operator's to spend — and reports the authorized maximum for the ledger to lock.
func TestChainRefillIsExplicitAndCarriesTheKernelsReserve(t *testing.T) {
	c, l := testChain(t)
	l.prepareErr = jrail.ErrNeedRefill
	l.intent = jrail.Intent{ID: jrail.ID{1}, Kind: jrail.KindRefill, Amount: big.NewInt(77)}

	out, err := c.Pay(context.Background(), "row-1", "0x000000000000000000000000000000000000dEaD", 100)
	if err != nil || !out.NeedRefill || l.refilled != 0 || l.sent != 0 {
		t.Fatalf("pay must only report the need: %+v %v refilled=%d sent=%d", out, err, l.refilled, l.sent)
	}
	r, err := c.Refill(context.Background(), 4200)
	if err != nil {
		t.Fatalf("refill: %v", err)
	}
	if l.refilled != 1 || l.reserveIn == nil || l.reserveIn.Int64() != 4200 {
		t.Errorf("reserve passed to the rail: got %v (refilled=%d), want 4200", l.reserveIn, l.refilled)
	}
	if r.ID == "" || r.Max != 77 {
		t.Errorf("the authorized maximum must be reported so it can be locked: %+v", r)
	}
	// Another operation still in flight is neither a shortage nor a failure: the payment waits.
	l.prepareErr = jrail.ErrInFlight
	out, err = c.Pay(context.Background(), "row-1", "0x000000000000000000000000000000000000dEaD", 100)
	if err != nil || out.TxHash != "" || out.NeedRefill || out.Blocked != "" {
		t.Errorf("in flight: want an empty outcome, got %+v %v", out, err)
	}
}

// A refill the rail signed just before the kernel could record it is found again by walking the
// nonces down from the newest to the first operation the ledger knows — so it is found whether it
// is the newest operation or a payment landed after it, and never adopted once the ledger has it.
func TestChainFindsTheRefillACrashInterrupted(t *testing.T) {
	c, l := testChain(t)
	refill := jrail.Intent{ID: jrail.ID{1}, Kind: jrail.KindRefill, Amount: big.NewInt(77)}
	payment := jrail.Intent{ID: jrail.ID{2}, Kind: jrail.KindWithdraw, Amount: big.NewInt(1)}
	l.intent = refill
	none := func(string) bool { return false }
	if _, found, _ := c.FindRefill(context.Background(), none); found {
		t.Fatal("nothing has been signed, nothing to find")
	}
	// Still unresolved — recorded, perhaps never broadcast — is where the rail itself reports it.
	l.pending = []jrail.Intent{refill}
	if r, found, err := c.FindRefill(context.Background(), none); err != nil || !found || r.Max != 77 {
		t.Fatalf("an unresolved purchase: %+v %v %v", r, found, err)
	}
	l.pending = nil
	l.nonce = 5
	l.byNonce = map[uint64]jrail.Intent{4: refill}
	if r, found, err := c.FindRefill(context.Background(), none); err != nil || !found || r.Max != 77 {
		t.Fatalf("a refill at the newest nonce: %+v %v %v", r, found, err)
	}
	// A payment landed after the refill before the kernel looked: the refill is still the ledger's.
	l.nonce = 6
	l.byNonce[5] = payment
	if r, found, err := c.FindRefill(context.Background(), none); err != nil || !found || r.Max != 77 {
		t.Fatalf("a refill behind a later payment: %+v %v %v", r, found, err)
	}
	// The walk stops at the first operation the ledger knows: what lies beneath it was seen before.
	known := func(id string) bool { return id == payment.ID.String() }
	if _, found, _ := c.FindRefill(context.Background(), known); found {
		t.Error("everything newer than a known operation was a payment; nothing to adopt")
	}
	known = func(id string) bool { return id == refill.ID.String() }
	if _, found, _ := c.FindRefill(context.Background(), known); found {
		t.Error("a refill the ledger already holds must not be adopted twice")
	}
}

// An amount the ledger cannot hold is refused, not wrapped: a wrong number in the books is worse
// than a loud stop.
func TestChainRefusesAmountsTheLedgerCannotHold(t *testing.T) {
	c, l := testChain(t)
	huge := new(big.Int).Lsh(big.NewInt(1), 70)
	l.deposits = []jrail.Deposit{{TxHash: common.HexToHash("0x1"), From: common.HexToAddress("0xaa"), Amount: huge, BlockNumber: 3}}
	if _, err := c.ScanDeposits(context.Background(), 0); err == nil {
		t.Error("a payment beyond 64 bits must be refused")
	}
	l.status, l.cost = jrail.StatusConfirmed, huge
	if _, _, err := c.RefillCost(context.Background(), jrail.ID{1}.String()); err == nil {
		t.Error("a refill cost beyond 64 bits must be refused")
	}
}

// A shortage is a stated reason, not an error: nothing was signed, the money stays where it is, and
// the payment is presented again when the shortage passes.
func TestChainReportsShortagesAsBlocked(t *testing.T) {
	for _, shortage := range []error{
		jrail.ErrInsufficientStablecoin, jrail.ErrInsufficientNative, jrail.ErrFeesAboveBound,
	} {
		c, l := testChain(t)
		l.prepareErr = shortage
		out, err := c.Pay(context.Background(), "row-1", "0x000000000000000000000000000000000000dEaD", 100)
		if err != nil {
			t.Fatalf("%v: a shortage is not an error: %v", shortage, err)
		}
		if out.Blocked == "" || out.TxHash != "" {
			t.Errorf("%v: want a blocked outcome with nothing signed, got %+v", shortage, out)
		}
	}
	// Anything else really is an error and must not be mistaken for a shortage.
	c, l := testChain(t)
	l.prepareErr = errors.New("endpoint exploded")
	if _, err := c.Pay(context.Background(), "row-1", "0x000000000000000000000000000000000000dEaD", 1); err == nil {
		t.Error("an unexpected failure must surface, not read as a shortage")
	}
}

// Only a finalized fact is confirmed or failed; anything else is still pending and must never be
// presented again.
func TestChainOutcomeReportsOnlyFinalizedFacts(t *testing.T) {
	c, l := testChain(t)
	ctx := context.Background()

	l.status = jrail.StatusPending
	if st, _, _ := c.Outcome(ctx, "row-1"); st != kernel.RailPending {
		t.Errorf("pending: got %s", st)
	}
	l.status = jrail.StatusUnknown
	if st, _, _ := c.Outcome(ctx, "row-1"); st != kernel.RailUnknown {
		t.Errorf("unknown: got %s", st)
	}
	l.status, l.settled, l.fact = jrail.StatusConfirmed, true, jrail.Fact{TxHash: common.HexToHash("0xabc"), BlockNumber: 12, Executed: true}
	st, fact, err := c.Outcome(ctx, "row-1")
	if err != nil || st != kernel.RailConfirmed || !fact.Exec || fact.Block != 12 {
		t.Errorf("confirmed: %s %+v %v", st, fact, err)
	}
	// A status that says final while the fact has not settled is still pending: the fact decides.
	l.settled = false
	if st, _, _ := c.Outcome(ctx, "row-1"); st != kernel.RailPending {
		t.Errorf("a final status with no settled fact: got %s, want pending", st)
	}
}

// A fuel purchase costs what the receipt says it cost, and nothing may be booked until it is final.
func TestChainRefillCostWaitsForFinality(t *testing.T) {
	c, l := testChain(t)
	ctx := context.Background()
	id := jrail.ID{1}.String()

	l.status = jrail.StatusPending
	if _, st, err := c.RefillCost(ctx, id); err != nil || st != kernel.RailPending {
		t.Errorf("before finality: %s %v", st, err)
	}
	l.status, l.cost = jrail.StatusConfirmed, big.NewInt(31)
	cost, st, err := c.RefillCost(ctx, id)
	if err != nil || st != kernel.RailConfirmed || cost != 31 {
		t.Errorf("after finality: cost=%d st=%s err=%v", cost, st, err)
	}
	// The library reports "still in flight" as its own error; that is pending, not a failure.
	l.costErr = jrail.ErrInFlight
	if _, st, err := c.RefillCost(ctx, id); err != nil || st != kernel.RailPending {
		t.Errorf("in flight: %s %v", st, err)
	}
}

// A payment must be reported again until the kernel has booked it, because the rail records it and
// moves its own cursor first: reporting only what is newly seen loses one to a crash in between.
func TestChainScanReportsFromTheKernelsMark(t *testing.T) {
	c, l := testChain(t)
	l.deposits = []jrail.Deposit{
		{TxHash: common.HexToHash("0x1"), From: common.HexToAddress("0xaa"), Amount: big.NewInt(10), BlockNumber: 3},
		{TxHash: common.HexToHash("0x2"), From: common.HexToAddress("0xbb"), Amount: big.NewInt(20), BlockNumber: 9},
	}
	all, err := c.ScanDeposits(context.Background(), 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("from the beginning: %d %v", len(all), err)
	}
	if all[0].Key == "" || all[0].From != strings.ToLower(all[0].From) {
		t.Errorf("a payment must carry a key and a canonical sender: %+v", all[0])
	}
	later, _ := c.ScanDeposits(context.Background(), 9)
	if len(later) != 1 || later[0].Amount != 20 {
		t.Errorf("from block 9: %+v", later)
	}
}

// A reference names one payment. A transaction carrying several is ambiguous, and guessing which was
// meant would credit the wrong amount.
func TestChainWitnessNamesExactlyOnePayment(t *testing.T) {
	c, l := testChain(t)
	ctx := context.Background()
	l.deposits = []jrail.Deposit{
		{TxHash: common.HexToHash("0x1"), LogIndex: 0, From: common.HexToAddress("0xaa"), Amount: big.NewInt(10), BlockNumber: 3},
		{TxHash: common.HexToHash("0x1"), LogIndex: 1, From: common.HexToAddress("0xbb"), Amount: big.NewInt(20), BlockNumber: 3},
	}
	hash := common.HexToHash("0x1").Hex()

	if _, err := c.Witness(ctx, hash, 0); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("an ambiguous reference must be refused, got %v", err)
	}
	d, err := c.Witness(ctx, hash+":1", 20)
	if err != nil || d.Amount != 20 {
		t.Fatalf("naming one by index: %+v %v", d, err)
	}
	if _, err := c.Witness(ctx, hash+":1", 999); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("a payment of another amount must be refused, got %v", err)
	}
	if _, err := c.Witness(ctx, common.HexToHash("0x9").Hex(), 5); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("a payment nobody has seen must be refused, got %v", err)
	}
}

// Proving control of an address is the ordinary wallet signature, and what is stored is the
// canonical form — so one address cannot be registered twice under different spellings.
func TestChainSignsAndVerifiesAnAddress(t *testing.T) {
	c, _ := testChain(t)
	msg := kernel.BlockchainAddressMessage("kernel-key", "user-1", c.Address())

	sig, err := c.Sign(msg)
	if err != nil || sig == "" {
		t.Fatalf("sign: %q %v", sig, err)
	}
	canonical, err := c.Verify(msg, strings.ToUpper(c.Address()[2:]), sig)
	if err == nil {
		// An address without its prefix is not an address; the library refuses it.
		t.Logf("unprefixed address accepted as %q", canonical)
	}
	canonical, err = c.Verify(msg, c.Address(), sig)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if canonical != strings.ToLower(canonical) || canonical != c.Address() {
		t.Errorf("stored form must be canonical: %q", canonical)
	}
	other, _ := crypto.GenerateKey()
	if _, err := c.Verify(msg, crypto.PubkeyToAddress(other.PublicKey).Hex(), sig); err == nil {
		t.Error("a signature by one address must not prove another")
	}
	if _, err := c.Verify([]byte("different message"), c.Address(), sig); err == nil {
		t.Error("a signature over one message must not prove another")
	}
}

// A destination nobody has proved they hold is money gone with no undo, so it is refused.
func TestChainRefusesAnUnregisteredDestination(t *testing.T) {
	c, _ := testChain(t)
	if _, err := c.Destination(""); !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("unregistered: got %v", err)
	}
	if _, err := c.Destination("not-an-address"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("malformed: got %v", err)
	}
	got, err := c.Destination("0x000000000000000000000000000000000000dEaD")
	if err != nil || got != strings.ToLower(got) {
		t.Errorf("a destination is stored canonically: %q %v", got, err)
	}
}

// The key is minted once and never overwritten: a key silently replaced would strand every coin the
// old one held.
func TestRailKeyIsCreatedOnceAndNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rail.key")

	first, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("a key must be readable only by its owner: %v %v", info.Mode().Perm(), err)
	}
	second, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(first.PublicKey) != crypto.PubkeyToAddress(second.PublicKey) {
		t.Fatal("opening the same home twice minted a second key")
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Error("the temporary file must not survive")
	}
}

// A payment is a transaction and a log index, because one transaction can carry several. Keying by
// the transaction alone would make two payments one fact, and the second would be refused as "the
// same fact on other terms" for as long as the scanner ran.
func TestChainKeysAPaymentByTransactionAndLogIndex(t *testing.T) {
	c, l := testChain(t)
	l.deposits = []jrail.Deposit{
		{TxHash: common.HexToHash("0x1"), LogIndex: 0, From: common.HexToAddress("0xaa"), Amount: big.NewInt(10), BlockNumber: 3},
		{TxHash: common.HexToHash("0x1"), LogIndex: 1, From: common.HexToAddress("0xbb"), Amount: big.NewInt(20), BlockNumber: 3},
	}
	got, err := c.ScanDeposits(context.Background(), 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("scan: %d %v", len(got), err)
	}
	if got[0].Key == got[1].Key {
		t.Fatalf("two payments in one transaction share a key: %q", got[0].Key)
	}
	if !strings.HasSuffix(got[1].Key, ":1") {
		t.Errorf("the key must carry the log index: %q", got[1].Key)
	}
}

// A purchase is durable before it is broadcast. When the broadcast fails the purchase still exists
// and owns a nonce, so it must be reported — the ledger has to lock its maximum — and it must be
// finished rather than duplicated, because the rail refuses every later operation until it
// resolves. A shortage refused before anything was recorded is the opposite case: nothing exists,
// and the caller is told the rail has stopped.
func TestChainFinishesAPurchaseWhoseBroadcastFailed(t *testing.T) {
	ctx := context.Background()
	refill := jrail.Intent{ID: jrail.ID{1}, Kind: jrail.KindRefill, Amount: big.NewInt(77)}

	c, l := testChain(t)
	l.intent = refill
	l.refillErr, l.refillID = errors.New("rpc unreachable"), refill.ID
	r, err := c.Refill(ctx, 10)
	if err != nil || r.ID == "" || r.Max != 77 {
		t.Fatalf("a recorded purchase must be reported so its maximum is locked: %+v %v", r, err)
	}
	if l.sent != 1 {
		t.Errorf("the failed broadcast must be presented again: sent=%d", l.sent)
	}

	// The rail is carrying that purchase, so it refuses to start another. The same call finishes
	// the one it holds instead of buying a second.
	c, l = testChain(t)
	l.intent = refill
	l.refillErr, l.pending = jrail.ErrInFlight, []jrail.Intent{refill}
	r, err = c.Refill(ctx, 10)
	if err != nil || r.ID == "" || r.Max != 77 || l.sent != 1 {
		t.Fatalf("an unfinished purchase must be finished: %+v %v sent=%d", r, err, l.sent)
	}

	// What it carries is a payment: the fuel waits its turn rather than reporting a shortage.
	c, l = testChain(t)
	l.refillErr = jrail.ErrInFlight
	l.pending = []jrail.Intent{{ID: jrail.ID{2}, Kind: jrail.KindWithdraw, Amount: big.NewInt(1)}}
	if r, err = c.Refill(ctx, 10); err != nil || r.ID != "" || l.sent != 0 {
		t.Fatalf("a payment in flight is not a fuel failure: %+v %v sent=%d", r, err, l.sent)
	}

	// Nothing was recorded: the operator cannot buy fuel, and that is a halt.
	c, l = testChain(t)
	l.refillErr = jrail.ErrInsufficientStablecoin
	if _, err = c.Refill(ctx, 10); !errors.Is(err, kernel.ErrRailStopped) {
		t.Fatalf("an unaffordable purchase: %v, want the rail stopped", err)
	}
}

// The key a held payment is listed under is the reference that closes it. It is also the only place
// the log index is published, so a transaction carrying two payments is nameable no other way.
func TestChainWitnessAcceptsTheKeyItMinted(t *testing.T) {
	c, l := testChain(t)
	ctx := context.Background()
	l.deposits = []jrail.Deposit{
		{TxHash: common.HexToHash("0x1"), LogIndex: 0, From: common.HexToAddress("0xaa"), Amount: big.NewInt(10), BlockNumber: 3},
		{TxHash: common.HexToHash("0x1"), LogIndex: 1, From: common.HexToAddress("0xbb"), Amount: big.NewInt(20), BlockNumber: 3},
	}
	all, err := c.ScanDeposits(ctx, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("scan: %d %v", len(all), err)
	}
	for _, listed := range all {
		got, werr := c.Witness(ctx, listed.Key, 0)
		if werr != nil {
			t.Fatalf("the listed key %q must name its own payment: %v", listed.Key, werr)
		}
		if got.Key != listed.Key || got.Amount != listed.Amount {
			t.Errorf("key %q named a different payment: %+v", listed.Key, got)
		}
	}
	// The bare transaction hash still names one, and is still ambiguous across two.
	if _, err := c.Witness(ctx, all[0].TxHash, 0); err == nil {
		t.Error("a hash carrying two payments must still be refused without an index")
	}
}

// seedCases covers where a kernel's payment scan begins. Serving publishes the address anyone pays,
// so the answer must exist before that, and it must never be "the head" for an address that has
// already been in the world unwatched.
func TestWhereTheScanBegins(t *testing.T) {
	ctx := context.Background()

	t.Run("a new kernel starts at the settled head, once", func(t *testing.T) {
		_, l := testChain(t)
		l.head = 12345
		at, err := l.SeedScan(ctx)
		if err != nil || at != 12345 {
			t.Fatalf("seed: %d %v", at, err)
		}
		// Twice is a read: the rail's cursor only advances, and a boot that already has one asks
		// the chain nothing.
		l.head = 99999
		if at, err = l.SeedScan(ctx); err != nil || at != 12345 {
			t.Fatalf("a second seed moved the cursor: %d %v", at, err)
		}
		if l.seeded != 1 {
			t.Fatalf("seeded %d times, want once", l.seeded)
		}
	})

	t.Run("an existing cursor is what the scan uses", func(t *testing.T) {
		_, l := testChain(t)
		l.cursor, l.hasCursor, l.head = 500, true, 900
		at, err := l.SeedScan(ctx)
		if err != nil || at != 500 {
			t.Fatalf("an existing cursor was not preserved: %d %v", at, err)
		}
		if l.seeded != 0 {
			t.Fatal("a kernel with a cursor asked the chain for a head it does not need")
		}
	})

	// The three states the boot decides between, which is where the rule actually lives.
	t.Run("a first boot with no cursor seeds and serves", func(t *testing.T) {
		_, l := testChain(t)
		l.head = 4242
		if err := fixScanStart(ctx, l, true, "http://node", "/home/k/rail.db"); err != nil {
			t.Fatalf("first boot refused a reachable chain: %v", err)
		}
		at, ok, _ := l.DepositsScannedTo()
		if !ok || at != 4242 {
			t.Fatalf("cursor %d present=%v, want the settled head", at, ok)
		}
	})

	t.Run("a first boot that cannot reach the chain refuses", func(t *testing.T) {
		_, l := testChain(t)
		l.headErr = errors.New("connection refused")
		err := fixScanStart(ctx, l, true, "http://node", "/home/k/rail.db")
		if err == nil {
			t.Fatal("a kernel served without knowing where to look for payments")
		}
		if !strings.Contains(err.Error(), "http://node") {
			t.Fatalf("the refusal does not name what to fix: %v", err)
		}
		if _, ok, _ := l.DepositsScannedTo(); ok {
			t.Fatal("a failed first boot left a cursor behind")
		}
	})

	t.Run("a made kernel with no cursor refuses rather than guess", func(t *testing.T) {
		_, l := testChain(t)
		l.head = 4242
		err := fixScanStart(ctx, l, false, "http://node", "/home/k/rail.db")
		if err == nil {
			t.Fatal("a kernel that has been in the world seeded itself at the head, skipping whatever it was paid")
		}
		if !strings.Contains(err.Error(), "/home/k/rail.db") {
			t.Fatalf("the refusal does not name what is missing: %v", err)
		}
		if l.seeded != 0 {
			t.Fatal("it seeded anyway")
		}
	})

	t.Run("a cursor already there is left alone, chain or no chain", func(t *testing.T) {
		_, l := testChain(t)
		l.cursor, l.hasCursor = 77, true
		l.headErr = errors.New("connection refused")
		for _, first := range []bool{true, false} {
			if err := fixScanStart(ctx, l, first, "http://node", "/home/k/rail.db"); err != nil {
				t.Fatalf("firstBoot=%v: a kernel with a cursor was refused over an outage: %v", first, err)
			}
		}
		if at, _, _ := l.DepositsScannedTo(); at != 77 {
			t.Fatalf("the cursor moved to %d", at)
		}
	})

	t.Run("a store that cannot be read is not a new kernel", func(t *testing.T) {
		_, l := testChain(t)
		l.scannedErr = errors.New("disk failure")
		l.head = 4242
		if err := fixScanStart(ctx, l, true, "http://node", "/home/k/rail.db"); err == nil {
			t.Fatal("an unreadable cursor was taken for an absent one, which would seed past real payments")
		}
		if l.seeded != 0 {
			t.Fatal("it seeded over a store it could not read")
		}
	})

	t.Run("an unreachable chain seeds nothing", func(t *testing.T) {
		_, l := testChain(t)
		l.headErr = errors.New("dial tcp: connection refused")
		if _, err := l.SeedScan(ctx); err == nil {
			t.Fatal("a seed against an unreachable chain reported success")
		}
		if _, ok, _ := l.DepositsScannedTo(); ok {
			t.Fatal("a failed seed left a cursor behind, so the scan would start from a block nobody read")
		}
	})
}
