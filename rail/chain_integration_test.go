// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

// The local-chain release gate (§8): the chain adaptor driven against a real chain, with
// independent chain reads as the oracle. Excluded from `go test ./...` because it needs anvil and
// juice-rail's compiled mock contracts.

package rail

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/daios-ai/juice/kernel"
)

const (
	anvilChainID = 31337
	// The token has six decimals, so one token is a million base units.
	tokenScale = 1_000_000
	// anvil finalizes two blocks behind the head, so three blocks settle whatever is pending.
	blocksToFinality = 3
	feeTier          = 500
	venuePrice       = 3000 * tokenScale

	// anvil's first account arrives with ether and signs every setup transaction. The sender is a
	// fresh key: it starts with nothing, so everything it holds was put there by this test.
	deployerKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	senderKeyHex   = "1111111111111111111111111111111111111111111111111111111111111111"
)

// wethPlaceholder labels the wrapped native token in the venue's calldata; the mock venue holds
// native currency directly, so nothing is ever wrapped.
var wethPlaceholder = common.HexToAddress("0x000000000000000000000000000000000000eeee")

var mockABI = func() abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(`[
      {"type":"function","name":"mint","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]},
      {"type":"function","name":"transfer","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"type":"bool"}]},
      {"type":"function","name":"balanceOf","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}],"stateMutability":"view"}]`))
	if err != nil {
		panic(err)
	}
	return parsed
}()

// chainFixture is one isolated chain: its own anvil, its own deployment, its own kernel home.
type chainFixture struct {
	t      *testing.T
	home   string
	rpcURL string
	client *ethclient.Client
	rpc    *rpc.Client
	token  common.Address
	router common.Address
}

func newChainFixture(t *testing.T) *chainFixture {
	t.Helper()
	if _, err := exec.LookPath("anvil"); err != nil {
		t.Skip("anvil is not on PATH: install Foundry to run the local-chain release gate")
	}
	f := &chainFixture{t: t, home: t.TempDir()}
	f.startAnvil()
	f.token = f.deploy("MockUSDT0", nil)
	f.router = f.deploy("MockRouter", ether(10),
		word(f.token), word(wethPlaceholder), wordInt(big.NewInt(feeTier)), wordInt(big.NewInt(venuePrice)))
	// Setup is over: from here the test decides when blocks appear, which is the only way to say
	// what is finalized and what is merely mined.
	f.call("evm_setAutomine", false)
	return f
}

// contractsOut locates juice-rail's compiled mocks. The published module carries their sources but
// not the artifacts, so only a checkout that has run `forge build` can supply them.
func contractsOut(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("JUICE_RAIL_CONTRACTS")
	if dir == "" {
		abs, err := filepath.Abs(filepath.Join("..", "..", "juice-rail", "contracts", "out"))
		if err != nil {
			t.Fatal(err)
		}
		dir = abs
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no compiled mock contracts at %s: run `forge build` in juice-rail/contracts, or set JUICE_RAIL_CONTRACTS", dir)
	}
	return dir
}

func (f *chainFixture) startAnvil() {
	f.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	f.rpcURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command("anvil", "--port", fmt.Sprint(port), "--chain-id", fmt.Sprint(anvilChainID),
		"--slots-in-an-epoch", "1", "--silent")
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		f.t.Fatalf("start anvil: %v", err)
	}
	f.t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		client, err := ethclient.Dial(f.rpcURL)
		if err == nil {
			if _, err := client.BlockNumber(context.Background()); err == nil {
				f.client = client
				f.t.Cleanup(client.Close)
				f.rpc, err = rpc.Dial(f.rpcURL)
				if err != nil {
					f.t.Fatal(err)
				}
				f.t.Cleanup(f.rpc.Close)
				return
			}
			client.Close()
		}
		if time.Now().After(deadline) {
			f.t.Fatal("anvil did not become ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *chainFixture) call(method string, args ...any) {
	f.t.Helper()
	if err := f.rpc.Call(nil, method, args...); err != nil {
		f.t.Fatalf("%s: %v", method, err)
	}
}

func (f *chainFixture) mine(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		f.call("evm_mine")
	}
}

// finalize mines the pending work and pushes it past finality, which is the only point at which
// the adaptor may report anything at all.
func (f *chainFixture) finalize() {
	f.t.Helper()
	f.mine(1 + blocksToFinality)
}

func (f *chainFixture) deploy(name string, value *big.Int, args ...[]byte) common.Address {
	f.t.Helper()
	artifact := filepath.Join(contractsOut(f.t), name+".sol", name+".json")
	raw, err := os.ReadFile(artifact)
	if err != nil {
		f.t.Fatalf("no creation code for %s: %v", name, err)
	}
	var a struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		f.t.Fatal(err)
	}
	code := common.FromHex(a.Bytecode.Object)
	for _, arg := range args {
		code = append(code, arg...)
	}
	receipt := f.send(deployerKeyHex, nil, code, value)
	if receipt.ContractAddress == (common.Address{}) {
		f.t.Fatalf("%s did not deploy", name)
	}
	return receipt.ContractAddress
}

// send signs a transaction, mines it, and returns its receipt. Every setup transaction lands at
// once; the test controls everything after that with mine and finalize.
func (f *chainFixture) send(keyHex string, to *common.Address, data []byte, value *big.Int) *types.Receipt {
	f.t.Helper()
	ctx := context.Background()
	key := mustHexKey(f.t, keyHex)
	from := crypto.PubkeyToAddress(key.PublicKey)

	nonce, err := f.client.PendingNonceAt(ctx, from)
	if err != nil {
		f.t.Fatal(err)
	}
	gasPrice, err := f.client.SuggestGasPrice(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	if value == nil {
		value = new(big.Int)
	}
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce: nonce, To: to, Value: value, Gas: 15_000_000,
		GasPrice: new(big.Int).Mul(gasPrice, big.NewInt(2)), Data: data,
	}), types.NewEIP155Signer(big.NewInt(anvilChainID)), key)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.client.SendTransaction(ctx, tx); err != nil {
		f.t.Fatalf("send transaction: %v", err)
	}
	f.call("evm_mine")
	deadline := time.Now().Add(20 * time.Second)
	for {
		receipt, err := f.client.TransactionReceipt(ctx, tx.Hash())
		if err == nil {
			return receipt
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("transaction %s was never mined", tx.Hash())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *chainFixture) mint(to common.Address, amount int64) {
	f.t.Helper()
	f.send(deployerKeyHex, &f.token, pack(f.t, "mint", to, big.NewInt(amount)), nil)
}

func (f *chainFixture) fund(to common.Address, wei *big.Int) {
	f.t.Helper()
	f.send(deployerKeyHex, &to, nil, wei)
}

// payIn is a token transfer from an outside wallet into an account, mined but not finalized.
func (f *chainFixture) payIn(keyHex string, to common.Address, amount int64) common.Hash {
	f.t.Helper()
	return f.send(keyHex, &f.token, pack(f.t, "transfer", to, big.NewInt(amount)), nil).TxHash
}

// tokenBalance is the oracle: a chain read that never passes through the rail.
func (f *chainFixture) tokenBalance(account common.Address) int64 {
	f.t.Helper()
	out, err := f.client.CallContract(context.Background(),
		ethereum.CallMsg{To: &f.token, Data: pack(f.t, "balanceOf", account)}, nil)
	if err != nil {
		f.t.Fatalf("balanceOf: %v", err)
	}
	return new(big.Int).SetBytes(out).Int64()
}

// transferCount says how many token transfers ever moved between two addresses, which is how a
// test asserts that money moved exactly once.
func (f *chainFixture) transferCount(from, to common.Address) int {
	f.t.Helper()
	logs, err := f.client.FilterLogs(context.Background(), ethereum.FilterQuery{
		FromBlock: big.NewInt(0),
		Addresses: []common.Address{f.token},
		Topics: [][]common.Hash{
			{crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))},
			{common.BytesToHash(from.Bytes())},
			{common.BytesToHash(to.Bytes())},
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(logs)
}

// world is the world file this chain answers to: the shipped shape, pointed at the deployment.
func (f *chainFixture) world() World {
	return World{
		Name: "anvil", Rail: RailEVM, ChainID: anvilChainID, Token: f.token.Hex(), Decimals: 6,
		RPC: f.rpcURL, Finality: "finalized",
		Venue: venueCfg{Router: f.router.Hex(), Quoter: f.router.Hex(), WrappedNative: wethPlaceholder.Hex(), FeeTier: feeTier},
		Gas: gasCfg{Min: "20000000000000000", Max: "50000000000000000", FeeBound: "10000000000000000",
			SlippageBps: 50, PaymentGas: 300000, SwapGas: 1500000},
	}
}

// open builds the adaptor the kernel would, in a home of its own.
func (f *chainFixture) open(w World, home string) (*Chain, error) {
	f.t.Helper()
	if home == "" {
		home = f.home
	}
	// A home with no rail database is a first boot: the scan is seeded at the head before anything
	// serves, which is what the kernel does.
	return OpenChain(context.Background(), w, home, true)
}

func pack(t *testing.T, method string, args ...any) []byte {
	t.Helper()
	out, err := mockABI.Pack(method, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustHexKey(t *testing.T, hexKey string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func word(a common.Address) []byte {
	var w [32]byte
	copy(w[12:], a.Bytes())
	return w[:]
}

func wordInt(v *big.Int) []byte { return common.LeftPadBytes(v.Bytes(), 32) }

func ether(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000_000_000_000_000))
}

// A kernel's rail key is its own and is minted on first use; a world that does not describe this
// chain is not one the kernel may hold money on.
func TestChainOpensOnARealChain(t *testing.T) {
	f := newChainFixture(t)

	c, err := f.open(f.world(), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, name := range []string{"rail.key", "rail.db"} {
		if _, err := os.Stat(filepath.Join(f.home, name)); err != nil {
			t.Fatalf("%s was not created: %v", name, err)
		}
	}
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("ready against a correct world: %v", err)
	}
	if got := c.Address(); got != strings.ToLower(got) || !strings.HasPrefix(got, "0x") {
		t.Fatalf("address %q is not a canonical lowercase address", got)
	}

	// The key is the account, so a second open of the same home must find the same one rather
	// than mint another and strand what the first holds.
	again, err := f.open(f.world(), "")
	if err != nil || again.Address() != c.Address() {
		t.Fatalf("reopen changed the account: %v %s vs %s", err, again.Address(), c.Address())
	}

	elsewhere := f.world()
	elsewhere.ChainID = anvilChainID + 1
	if _, err := f.open(elsewhere, t.TempDir()); err == nil {
		t.Fatal("a world naming another chain was accepted")
	} else if !strings.Contains(err.Error(), "chain") {
		t.Fatalf("wrong-chain refusal does not name the chain: %v", err)
	}

	// An address that is not the token cannot be checked, so the rail never becomes ready and
	// every money verb refuses (D23).
	notAToken := f.world()
	notAToken.Token = f.router.Hex()
	blind, err := f.open(notAToken, t.TempDir())
	if err == nil {
		err = blind.Ready(context.Background())
	}
	if err == nil {
		t.Fatal("a world naming a non-token address was accepted")
	}
}

// Money in is credited from finalized facts alone, exactly as it was sent, and the kernel's own
// mark decides what is new — so re-scanning reports the same payments again.
func TestChainSeesOnlyFinalizedDeposits(t *testing.T) {
	f := newChainFixture(t)
	c, err := f.open(f.world(), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	vault := common.HexToAddress(c.Address())
	sender := crypto.PubkeyToAddress(mustHexKey(t, senderKeyHex).PublicKey)
	f.mint(sender, 100*tokenScale)
	f.fund(sender, ether(1))

	settled := f.payIn(senderKeyHex, vault, 25*tokenScale)
	f.finalize()
	inFlight := f.payIn(senderKeyHex, vault, 7*tokenScale)

	first, err := c.ScanDeposits(ctx, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("want only the finalized payment, got %d", len(first))
	}
	d := first[0]
	if d.TxHash != settled.Hex() || d.Amount != 25*tokenScale {
		t.Fatalf("payment misreported: %+v", d)
	}
	if d.From != strings.ToLower(sender.Hex()) {
		t.Fatalf("sender %q is not the canonical lowercase %q", d.From, strings.ToLower(sender.Hex()))
	}
	if d.Block == 0 {
		t.Fatal("payment reports no block")
	}

	// The rail's cursor has moved past it, but the kernel has not marked it booked, so it must
	// still be reported: a crash between those two writes would otherwise lose the payment.
	again, err := c.ScanDeposits(ctx, 0)
	if err != nil || len(again) != 1 || again[0].Key != d.Key {
		t.Fatalf("re-scan lost the payment: %v %+v", err, again)
	}

	// Witness names the same payment by the transaction that carried it.
	w, err := c.Witness(ctx, settled.Hex(), 25*tokenScale)
	if err != nil {
		t.Fatalf("witness: %v", err)
	}
	if w.Key != d.Key || w.Amount != d.Amount || w.From != d.From {
		t.Fatalf("witness disagrees with the scan: %+v vs %+v", w, d)
	}
	if _, err := c.Witness(ctx, inFlight.Hex(), 0); err == nil {
		t.Fatal("a payment nobody has observed was witnessed")
	}

	f.finalize()
	both, err := c.ScanDeposits(ctx, 0)
	if err != nil || len(both) != 2 {
		t.Fatalf("want both payments once finalized, got %d (%v)", len(both), err)
	}

	// The audit's cut: what is held at a block that can no longer change, and how far payments
	// have been looked for. A cursor behind the cut would mean money on the chain and not in the
	// ledger.
	token, _, block, ok, err := c.FinalizedBalances(ctx)
	if err != nil || !ok {
		t.Fatalf("finalized balances: %v", err)
	}
	if token != 32*tokenScale {
		t.Fatalf("vault holds %d, %d was sent", token, 32*tokenScale)
	}
	scanned, ok, err := c.DepositsScannedTo()
	if err != nil || !ok {
		t.Fatalf("scanned-to: %v", err)
	}
	if scanned != block {
		t.Fatalf("payments observed to %d but audited at %d", scanned, block)
	}
}

// Money out moves the token once, and is confirmed only when it can no longer be undone.
func TestChainPaysOnceAndConfirmsAtFinality(t *testing.T) {
	f := newChainFixture(t)
	c, err := f.open(f.world(), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	vault := common.HexToAddress(c.Address())
	f.mint(vault, 50*tokenScale)
	f.fund(vault, ether(1))
	payee := crypto.PubkeyToAddress(mustHexKey(t, senderKeyHex).PublicKey)

	const id = "9f4d2b1e-0000-4000-8000-000000000001"
	out, err := c.Pay(ctx, id, payee.Hex(), 10*tokenScale)
	if err != nil {
		t.Fatalf("pay: %v", err)
	}
	if out.TxHash == "" || out.NeedRefill || out.Blocked != "" {
		t.Fatalf("want a plain payment, got %+v", out)
	}

	f.mine(1)
	if st, _, err := c.Outcome(ctx, id); err != nil || st != kernel.RailPending {
		t.Fatalf("a mined payment must stay pending until finality: %v %v", st, err)
	}
	f.mine(blocksToFinality)

	st, fact, err := c.Outcome(ctx, id)
	if err != nil || st != kernel.RailConfirmed {
		t.Fatalf("want confirmed once finalized, got %v (%v)", st, err)
	}
	if fact.TxHash != out.TxHash || !fact.Exec || fact.Block == 0 {
		t.Fatalf("fact does not describe the payment: %+v", fact)
	}
	if got := f.tokenBalance(payee); got != 10*tokenScale {
		t.Fatalf("payee holds %d, %d was paid", got, 10*tokenScale)
	}

	// Presenting the same payment again is safe: a reply lost in transit must never pay twice.
	if _, err := c.Pay(ctx, id, payee.Hex(), 10*tokenScale); err != nil {
		t.Fatalf("re-presenting the payment: %v", err)
	}
	f.finalize()
	if got := f.tokenBalance(payee); got != 10*tokenScale {
		t.Fatalf("payee holds %d after a second presentation", got)
	}
	if n := f.transferCount(vault, payee); n != 1 {
		t.Fatalf("the token moved %d times", n)
	}
	if token, _, _, _, err := c.FinalizedBalances(ctx); err != nil || token != 40*tokenScale {
		t.Fatalf("vault holds %d after paying 10 of 50: %v", token, err)
	}
}

// A kernel proves its own address with its own key, and a signature by anyone else proves nothing.
func TestChainSignsWithItsRealKey(t *testing.T) {
	f := newChainFixture(t)
	c, err := f.open(f.world(), "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	other, err := f.open(f.world(), t.TempDir())
	if err != nil {
		t.Fatalf("open second kernel: %v", err)
	}

	msg := []byte("juice kernel rail address\nkernel: k\nnetwork: n\naddress: " + c.Address())
	sig, err := c.Sign(msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := c.Verify(msg, c.Address(), sig)
	if err != nil || got != c.Address() {
		t.Fatalf("own signature did not verify: %q %v", got, err)
	}

	theirs, err := other.Sign(msg)
	if err != nil {
		t.Fatalf("sign as another kernel: %v", err)
	}
	if _, err := c.Verify(msg, c.Address(), theirs); err == nil {
		t.Fatal("a signature by another key was accepted for this address")
	}
}
