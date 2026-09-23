// SPDX-License-Identifier: AGPL-3.0-only

package rail

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	jrail "github.com/daios-ai/juice-rail/go/rail"
	jsqlite "github.com/daios-ai/juice-rail/go/sqlite"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/daios-ai/juice/kernel"
)

// lib is the part of juice-rail this adaptor drives. Naming it here keeps the adaptor testable
// without a chain, and keeps the dependency visible: everything the kernel's money rules need from
// the outside world is these sixteen calls.
type lib interface {
	Account() common.Address
	CheckDomain(ctx context.Context) error
	// WrappedNative is what the venue's router says it unwraps into fuel — its own WETH9() — which
	// the world must name as the token the refill buys.
	WrappedNative(ctx context.Context) (common.Address, error)
	Prepare(ctx context.Context, id jrail.ID, kind jrail.Kind, to common.Address, amount *big.Int) error
	Send(ctx context.Context, id jrail.ID) (common.Hash, error)
	Refill(ctx context.Context, reserve *big.Int) (jrail.ID, common.Hash, error)
	Status(ctx context.Context, id jrail.ID) (jrail.Status, error)
	Outcome(ctx context.Context, id jrail.ID) (jrail.Fact, bool, error)
	RefillCost(ctx context.Context, id jrail.ID) (*big.Int, error)
	Intent(id jrail.ID) (jrail.Intent, bool, error)
	ScanDeposits(ctx context.Context) ([]jrail.Deposit, error)
	Deposits() ([]jrail.Deposit, error)
	DepositsScannedTo() (uint64, bool, error)
	// SeedScan fixes where this account's payment scan begins, once: the block the chain reports
	// settled the first time it is reached. An address cannot have been paid before it existed, so
	// everything below that block is empty, and walking it is work that grows by a day every day.
	SeedScan(ctx context.Context) (uint64, error)
	SettledBalances(ctx context.Context) (*big.Int, *big.Int, uint64, error)
	// The three below are how a purchase the ledger never recorded is found again: unresolved ones
	// the rail still holds, and, once resolved, the nonce each intent owns.
	Pending() ([]jrail.Intent, error)
	Nonce(ctx context.Context) (uint64, error)
	IntentByNonce(nonce uint64) (jrail.Intent, bool, error)
}

// railLib is juice-rail with the two reads the adaptor needs that the library keeps on its store and
// its chain client rather than on the rail itself.
type railLib struct {
	*jrail.Rail
	store  jrail.Store
	client *ethclient.Client
}

func (l railLib) Nonce(ctx context.Context) (uint64, error) {
	return l.client.PendingNonceAt(ctx, l.Account())
}

func (l railLib) IntentByNonce(nonce uint64) (jrail.Intent, bool, error) {
	return l.store.IntentByNonce(l.Account(), nonce)
}

func (l railLib) WrappedNative(ctx context.Context) (common.Address, error) {
	router := l.Domain().Venue.Router
	out, err := l.client.CallContract(ctx, ethereum.CallMsg{To: &router, Data: crypto.Keccak256([]byte("WETH9()"))[:4]}, nil)
	if err != nil {
		return common.Address{}, err
	}
	if len(out) != common.HashLength {
		return common.Address{}, fmt.Errorf("router %s does not answer WETH9()", router)
	}
	return common.BytesToAddress(out), nil
}

// SeedScan records the settled head as this account's scan cursor, unless it already has one. The
// cursor is the rail's own durable record and only ever advances, so a repeat is a read.
func (l railLib) SeedScan(ctx context.Context) (uint64, error) {
	if at, ok, err := l.store.Cursor(l.Account()); err != nil {
		return 0, err
	} else if ok {
		return at, nil
	}
	// The domain's tag as a negative block number is how the settled head is named, the same read
	// the rail makes of it everywhere else.
	head, err := l.client.HeaderByNumber(ctx, big.NewInt(int64(l.Domain().Finality)))
	if err != nil {
		return 0, fmt.Errorf("read settled head: %w", err)
	}
	if head == nil {
		return 0, fmt.Errorf("read settled head: no header")
	}
	at := head.Number.Uint64()
	if err := l.store.PutCursor(l.Account(), at); err != nil {
		return 0, err
	}
	return at, nil
}

// Chain is the adaptor over juice-rail. It translates the rail's outcomes and adds none of its own:
// every fact it reports came from a finalized chain read.
type Chain struct {
	rail  lib
	venue jrail.Venue
	key   *ecdsa.PrivateKey
	addr  common.Address

	mu      sync.Mutex
	checked bool
}

// Open builds the rail a world calls for. The world says which one it settles on, so nothing
// anywhere infers it from which fields the file happens to carry.
func Open(ctx context.Context, w World, home string, firstBoot bool) (kernel.Rail, error) {
	switch w.Rail {
	case RailManual:
		return NewManual(), nil
	case RailEVM:
		return OpenChain(ctx, w, home, firstBoot)
	}
	return nil, fmt.Errorf("world %q names rail %q", w.Name, w.Rail)
}

// OpenChain wires juice-rail to this kernel's own key and records, both kept beside the ledger they
// belong to. A domain that is wrong rather than merely unreachable refuses the boot: money sent on
// the wrong chain is simply gone.
func OpenChain(ctx context.Context, w World, home string, firstBoot bool) (*Chain, error) {
	domain, err := w.Domain()
	if err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(home, "rail.key"))
	if err != nil {
		return nil, err
	}
	store, err := jsqlite.Open(filepath.Join(home, "rail.db"), jrail.DomainKey(domain.ChainID, domain.Token))
	if err != nil {
		return nil, err
	}
	client, err := ethclient.DialContext(ctx, w.RPC)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", w.RPC, err)
	}
	r, err := jrail.New(domain, store, client, key)
	if err != nil {
		return nil, err
	}
	c := &Chain{rail: railLib{Rail: r, store: store, client: client}, venue: domain.Venue, key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}
	// Creating a kernel is when the network is bound for life and the address anyone pays becomes
	// public, so the chain answers for all of it or there is no kernel: the chain it claims to be,
	// the token at the address named, and the fuel that sends a payment. A kernel that already
	// exists is past that question — a chain it cannot reach only makes money wait (D23).
	verified := c.Ready(ctx)
	if verified != nil {
		if firstBoot || errors.Is(verified, jrail.ErrWrongDomain) || errors.Is(verified, jrail.ErrBadInput) {
			return nil, verified
		}
	}
	if err := fixScanStart(ctx, c.rail, firstBoot, w.RPC, filepath.Join(home, "rail.db")); err != nil {
		return nil, err
	}
	return c, nil
}

// fixScanStart settles where this kernel looks for payments, before it serves. Serving publishes
// the address anyone pays, and dialling an endpoint is not reaching it, so a kernel whose node is
// down gets as far as here with nothing recorded. Three states, and only the first is ordinary:
//
//   - a cursor exists: the scan continues from it, and an endpoint that is down costs nothing but
//     a wait, as it did before;
//   - none, and this is the kernel's first boot: the settled head becomes the cursor, or the boot
//     fails and is run again — a kernel that never served was never paid;
//   - none, and the kernel is already made: it has had an address in the world with nobody
//     watching, so there is no honest answer and it refuses.
//
// Whether the chain answered for itself is settled in OpenChain before this runs: a first boot that
// failed its check never reaches here, so a seed only ever follows a verified chain.
func fixScanStart(ctx context.Context, l lib, firstBoot bool, endpoint, records string) error {
	_, seeded, err := l.DepositsScannedTo()
	if err != nil {
		return err
	}
	if seeded {
		return nil
	}
	if !firstBoot {
		return fmt.Errorf("this kernel has a payment address but no record of where to look for "+
			"payments, so one already made would be missed: restore %s, or serve a new kernel", records)
	}
	if _, err := l.SeedScan(ctx); err != nil {
		return fmt.Errorf("first boot must reach %s to fix where payments are looked for: %w", endpoint, err)
	}
	return nil
}

// newChain builds an adaptor over an arbitrary library implementation, for tests.
func newChain(l lib, key *ecdsa.PrivateKey) *Chain {
	return &Chain{rail: l, key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}
}

// loadOrCreateKey reads the rail key, minting one on first use. The new key is written to a
// temporary file and linked into place, which fails if a key is already there: a key silently
// overwritten would strand every coin the old one held.
func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		return jrail.ParseKey(strings.TrimSpace(string(b)), path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	// The key reaches the disk before it has a name: a file that exists but is empty after a power
	// cut would be indistinguishable from a key, and every coin the vault holds would be stranded.
	tmp := path + ".new"
	if err := writeSynced(tmp, hex.EncodeToString(crypto.FromECDSA(key))); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		// Somebody else created it between the read and now; theirs wins.
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, err
		}
		return jrail.ParseKey(strings.TrimSpace(string(b)), path)
	}
	// The name is a directory entry of its own; until the directory is flushed the key could exist
	// on disk under no name at all.
	return key, syncDir(filepath.Dir(path))
}

// syncDir waits for a directory's entries to reach the disk.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// writeSynced writes one file and waits for the disk to say so, directory entry included.
func writeSynced(path, body string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// Ready runs the full domain check — chain, token, decimals, venue — and remembers that it passed.
// Until it does, the kernel does no rail work at all: an unverified token misstates every amount by
// whatever its decimals turn out to be. The venue check includes what the router unwraps: a refill
// buys the token the world names, and if the router unwraps another, the purchase stays inside it.
func (c *Chain) Ready(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checked {
		return nil
	}
	if err := c.rail.CheckDomain(ctx); err != nil {
		return err
	}
	unwraps, err := c.rail.WrappedNative(ctx)
	if err != nil {
		return fmt.Errorf("swap venue %s: %w", c.venue.Router, err)
	}
	if unwraps != c.venue.WETH {
		return fmt.Errorf("%w: swap venue %s unwraps %s, the world names %s", jrail.ErrWrongDomain, c.venue.Router, unwraps, c.venue.WETH)
	}
	c.checked = true
	return nil
}

// Address is this kernel's own, where anyone pays it.
func (c *Chain) Address() string { return strings.ToLower(c.addr.Hex()) }

// Destination refuses to pay a party that has registered nowhere: money sent to an address nobody
// proved they hold is gone, and there is no undo.
func (c *Chain) Destination(registered string) (string, error) {
	if registered == "" {
		return "", kernel.ErrInvalidState.Wrap(
			"no payment address is registered; register the one you control first:  juice user address ADDRESS")
	}
	addr, err := jrail.ParseAddress(registered, "destination")
	if err != nil {
		return "", kernel.ErrInvalidInput.Wrap(err.Error())
	}
	return strings.ToLower(addr.Hex()), nil
}

// Pay presents one payment. It uses the rail's separate steps rather than its combined verb: when
// fuel is short it must sign nothing and say so, so that the kernel can record the refill it is
// about to ask for before any refill exists.
func (c *Chain) Pay(ctx context.Context, id, to string, amount int64) (kernel.RailOutcome, error) {
	rid, err := railID(id)
	if err != nil {
		return kernel.RailOutcome{}, err
	}
	dest, err := jrail.ParseAddress(to, "destination")
	if err != nil {
		return kernel.RailOutcome{}, kernel.ErrInvalidInput.Wrap(err.Error())
	}
	err = c.rail.Prepare(ctx, rid, jrail.KindWithdraw, dest, big.NewInt(amount))
	switch {
	case errors.Is(err, jrail.ErrNeedRefill):
		return kernel.RailOutcome{NeedRefill: true}, nil
	case errors.Is(err, jrail.ErrInFlight):
		// Another operation, ours or a refill, is still on its way; the payment waits its turn.
		return kernel.RailOutcome{}, nil
	case err != nil:
		return c.blocked(err)
	}
	hash, err := c.rail.Send(ctx, rid)
	if err != nil {
		return c.blocked(err)
	}
	return kernel.RailOutcome{TxHash: hash.Hex()}, nil
}

// Refill buys fuel with the operator's own stablecoin, leaving reserve — what is not the operator's
// to spend — untouched, which the rail's one rule enforces before signing. The purchase is durable
// before it is broadcast, so a broadcast that fails leaves a real purchase that owns a nonce: the
// rail then refuses every later operation until it resolves, and presenting it again is what gets
// it out. That is what this does when the rail says something is already in flight, so a stalled
// purchase is finished rather than duplicated. An empty refill with no error means the thing in
// flight is a payment, and the fuel waits its turn.
func (c *Chain) Refill(ctx context.Context, reserve int64) (kernel.RailRefill, error) {
	id, _, err := c.rail.Refill(ctx, big.NewInt(reserve))
	if errors.Is(err, jrail.ErrInFlight) {
		in, held, ferr := c.heldRefill()
		if ferr != nil || !held {
			return kernel.RailRefill{}, ferr
		}
		id, err = in.ID, nil
	}
	if id == (jrail.ID{}) {
		if out, berr := c.blocked(err); berr == nil {
			return kernel.RailRefill{}, kernel.ErrRailStopped.Wrap(out.Blocked)
		}
		return kernel.RailRefill{}, err
	}
	// The purchase exists from here on, whether or not this attempt reached the chain. Sending is
	// idempotent — the same nonce, the same terms — and does nothing to one already resolved.
	if _, serr := c.rail.Send(ctx, id); serr != nil && err == nil {
		err = serr
	}
	r, rerr := c.refill(id)
	if rerr != nil {
		return kernel.RailRefill{}, rerr
	}
	// A failed broadcast is reported, but the purchase is reported with it: the ledger must lock its
	// maximum either way, or the rail would hold fuel the books never paid for.
	return r, nil
}

// FindRefill returns a purchase the rail holds that the ledger does not: one made just before a
// crash could record it. It looks where such a purchase can be. While unresolved it is what the
// rail itself reports as outstanding; once resolved it is filed under the nonce it owns, and
// walking down from the newest nonce stops at the first operation the ledger already holds, since
// everything older has been seen. Nothing is inferred from position alone: a purchase is adopted
// because the ledger has no row for it.
func (c *Chain) FindRefill(ctx context.Context, known func(id string) bool) (kernel.RailRefill, bool, error) {
	if in, held, err := c.heldRefill(); err != nil {
		return kernel.RailRefill{}, false, err
	} else if held && !known(in.ID.String()) {
		r, err := c.refill(in.ID)
		return r, err == nil, err
	}
	nonce, err := c.rail.Nonce(ctx)
	if err != nil {
		return kernel.RailRefill{}, false, err
	}
	for ; nonce > 0; nonce-- {
		in, ok, err := c.rail.IntentByNonce(nonce - 1)
		if err != nil {
			return kernel.RailRefill{}, false, err
		}
		if !ok || known(in.ID.String()) {
			break
		}
		if in.Kind == jrail.KindRefill {
			r, err := c.refill(in.ID)
			return r, err == nil, err
		}
	}
	return kernel.RailRefill{}, false, nil
}

// heldRefill is the fuel purchase the rail is still carrying, if the thing it carries is one.
func (c *Chain) heldRefill() (jrail.Intent, bool, error) {
	pending, err := c.rail.Pending()
	if err != nil {
		return jrail.Intent{}, false, err
	}
	for _, in := range pending {
		if in.Kind == jrail.KindRefill {
			return in, true, nil
		}
	}
	return jrail.Intent{}, false, nil
}

func (c *Chain) refill(id jrail.ID) (kernel.RailRefill, error) {
	in, ok, err := c.rail.Intent(id)
	if err != nil || !ok {
		return kernel.RailRefill{}, kernel.ErrNotFound.Wrapf("no refill %s", id)
	}
	max, err := toInt64(in.Amount)
	if err != nil {
		return kernel.RailRefill{}, err
	}
	return kernel.RailRefill{ID: id.String(), Max: max}, nil
}

// toInt64 refuses an amount the ledger cannot hold. Wrapping it would book a wrong number and a
// negative one would jam the scanner on a payment it can never represent; an error is the truth.
func toInt64(v *big.Int) (int64, error) {
	if v == nil || !v.IsInt64() || v.Sign() < 0 {
		return 0, kernel.ErrInvalidState.Wrapf("amount %v cannot be held by the ledger", v)
	}
	return v.Int64(), nil
}

// blocked turns a shortage into a stated reason rather than an error. Blocked is not lost: the row
// keeps its money and is presented again, and the halt lifts when the shortage does.
func (c *Chain) blocked(err error) (kernel.RailOutcome, error) {
	for _, shortage := range []error{jrail.ErrInsufficientStablecoin, jrail.ErrInsufficientNative, jrail.ErrFeesAboveBound} {
		if errors.Is(err, shortage) {
			return kernel.RailOutcome{Blocked: err.Error()}, nil
		}
	}
	return kernel.RailOutcome{}, err
}

// Outcome reports where a payment stands, from finalized facts only.
func (c *Chain) Outcome(ctx context.Context, id string) (kernel.RailStatus, kernel.RailFact, error) {
	rid, err := railID(id)
	if err != nil {
		return kernel.RailUnknown, kernel.RailFact{}, err
	}
	st, err := c.rail.Status(ctx, rid)
	if err != nil {
		return kernel.RailUnknown, kernel.RailFact{}, err
	}
	status := railStatus(st)
	if status != kernel.RailConfirmed && status != kernel.RailFailed {
		return status, kernel.RailFact{}, nil
	}
	fact, settled, err := c.rail.Outcome(ctx, rid)
	if err != nil || !settled {
		return kernel.RailPending, kernel.RailFact{}, err
	}
	return status, kernel.RailFact{TxHash: fact.TxHash.Hex(), Block: fact.BlockNumber, Exec: fact.Executed}, nil
}

// RefillCost is what a fuel purchase actually consumed, read from the receipt that carried it. A
// purchase that reverted consumed nothing: it burned fuel, which is not money.
func (c *Chain) RefillCost(ctx context.Context, refillID string) (int64, kernel.RailStatus, error) {
	rid, err := jrail.ParseID(refillID)
	if err != nil {
		return 0, kernel.RailUnknown, kernel.ErrInvalidInput.Wrap(err.Error())
	}
	st, err := c.rail.Status(ctx, rid)
	if err != nil {
		return 0, kernel.RailUnknown, err
	}
	status := railStatus(st)
	if status != kernel.RailConfirmed && status != kernel.RailFailed {
		return 0, status, nil
	}
	cost, err := c.rail.RefillCost(ctx, rid)
	if err != nil {
		if errors.Is(err, jrail.ErrInFlight) {
			return 0, kernel.RailPending, nil
		}
		return 0, status, err
	}
	n, err := toInt64(cost)
	return n, status, err
}

// ScanDeposits observes payments in, then reports every one at or after sinceBlock. The rail records
// a payment and advances its own cursor before the kernel books it, so reporting only what was newly
// seen would lose a payment to a crash between those two writes.
func (c *Chain) ScanDeposits(ctx context.Context, sinceBlock uint64) ([]kernel.RailDeposit, error) {
	if _, err := c.rail.ScanDeposits(ctx); err != nil {
		return nil, err
	}
	all, err := c.rail.Deposits()
	if err != nil {
		return nil, err
	}
	out := make([]kernel.RailDeposit, 0, len(all))
	for _, d := range all {
		if d.BlockNumber < sinceBlock {
			continue
		}
		dep, err := toDeposit(d)
		if err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, nil
}

// Witness resolves a transaction the operator named into the payment it carried. A transaction
// carrying several payments is named with its index, since only one of them is the one meant.
func (c *Chain) Witness(_ context.Context, ref string, amount int64) (kernel.RailDeposit, error) {
	// A held payment is listed under the key toDeposit mints, so that key is a name for it: the
	// operator hands back what the kernel showed them. It is also the only place the log index is
	// published, so a transaction carrying two payments is nameable no other way.
	hash, idx, hasIdx := strings.Cut(strings.TrimPrefix(ref, "rail:"), ":")
	all, err := c.rail.Deposits()
	if err != nil {
		return kernel.RailDeposit{}, err
	}
	var found []jrail.Deposit
	for _, d := range all {
		if !strings.EqualFold(d.TxHash.Hex(), hash) {
			continue
		}
		if hasIdx {
			if n, cerr := strconv.ParseUint(idx, 10, 32); cerr != nil || uint(n) != d.LogIndex {
				continue
			}
		}
		found = append(found, d)
	}
	switch {
	case len(found) == 0:
		return kernel.RailDeposit{}, kernel.ErrNotFound.Wrapf("no finalized payment %s has been observed", ref)
	case len(found) > 1:
		return kernel.RailDeposit{}, kernel.ErrInvalidInput.Wrapf("%s carried %d payments; name one by index", hash, len(found))
	}
	d, err := toDeposit(found[0])
	if err != nil {
		return kernel.RailDeposit{}, err
	}
	if amount > 0 && d.Amount != amount {
		return kernel.RailDeposit{}, kernel.ErrInvalidInput.Wrapf("payment %s is %d, not %d", ref, d.Amount, amount)
	}
	return d, nil
}

func toDeposit(d jrail.Deposit) (kernel.RailDeposit, error) {
	amount, err := toInt64(d.Amount)
	if err != nil {
		return kernel.RailDeposit{}, err
	}
	return kernel.RailDeposit{
		Key:    fmt.Sprintf("rail:%s:%d", d.TxHash.Hex(), d.LogIndex),
		TxHash: d.TxHash.Hex(),
		From:   strings.ToLower(d.From.Hex()),
		Amount: amount,
		Block:  d.BlockNumber,
	}, nil
}

// FinalizedBalances is the audit's cut: what is held at a block that can no longer change.
func (c *Chain) FinalizedBalances(ctx context.Context) (int64, string, uint64, bool, error) {
	token, gas, block, err := c.rail.SettledBalances(ctx)
	if err != nil {
		return 0, "", 0, false, err
	}
	held, err := toInt64(token)
	if err != nil {
		return 0, "", 0, false, err
	}
	return held, jrail.FormatNative(gas), block, true, nil
}

// DepositsScannedTo says how far payments have been observed, so an audit knows whether its cut is
// covered or whether money has arrived that nobody has looked for yet.
func (c *Chain) DepositsScannedTo() (uint64, bool, error) { return c.rail.DepositsScannedTo() }

// Sign proves this kernel controls its own address, in the ordinary personal-message form a wallet
// produces, so anyone can check it the same way.
func (c *Chain) Sign(msg []byte) (string, error) {
	sig, err := crypto.Sign(accountsHash(msg), c.key)
	if err != nil {
		return "", err
	}
	sig[64] += 27 // the V value wallets use
	return "0x" + hex.EncodeToString(sig), nil
}

// Verify recovers the signer and reports the address in canonical form — the only form stored or
// compared, so one address cannot be registered twice under different spellings.
func (c *Chain) Verify(msg []byte, address, sig string) (string, error) {
	want, err := jrail.ParseAddress(address, "address")
	if err != nil {
		return "", kernel.ErrInvalidInput.Wrap(err.Error())
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(sig, "0x"))
	if err != nil || len(raw) != 65 {
		return "", kernel.ErrInvalidInput.Wrap("signature must be 65 bytes of hex")
	}
	if raw[64] >= 27 {
		raw[64] -= 27
	}
	pub, err := crypto.SigToPub(accountsHash(msg), raw)
	if err != nil {
		return "", kernel.ErrUnauthorized.Wrap("signature does not recover a signer")
	}
	got := crypto.PubkeyToAddress(*pub)
	if got != want {
		return "", kernel.ErrUnauthorized.Wrap("signature was not made by that address")
	}
	return strings.ToLower(got.Hex()), nil
}

// accountsHash is the personal-message hash wallets sign, which is what makes a signature produced
// in a browser verifiable here.
func accountsHash(msg []byte) []byte {
	return crypto.Keccak256([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(msg))), msg)
}

// railID turns the kernel's own uuid into the rail's identifier, so a retry carries the same
// at-most-once guarantee end to end.
func railID(id string) (jrail.ID, error) {
	sum := crypto.Keccak256([]byte("juice-rail-op|" + id))
	var out jrail.ID
	copy(out[:], sum)
	return out, nil
}

func railStatus(s jrail.Status) kernel.RailStatus {
	switch s {
	case jrail.StatusConfirmed:
		return kernel.RailConfirmed
	case jrail.StatusFailed:
		return kernel.RailFailed
	case jrail.StatusPending:
		return kernel.RailPending
	default:
		return kernel.RailUnknown
	}
}
