package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Rail is the world's money, and it is the only thing a world is allowed to change.
//
// The point of running one economy over three rails is that the reports are comparable. That fails
// the moment the rail decides who trades with whom: two runs then describe two different economies
// under one name. So a Rail may decide only how money enters, how a payment is made and becomes
// final, how long to wait for it, and what the run cost. Participants, actions, prices, trades,
// compositions, attacks, assertions and the number of trading rounds come from the story and are
// identical everywhere. Rounds cost no gas: a cross-kernel call accumulates a debt and the debt is
// paid once.
type Rail interface {
	Name() string
	// Prepare runs before any kernel starts, given what the story will ask for, so a rail with a
	// spending limit refuses in advance instead of running dry halfway.
	Prepare(n *Net, s Shape) error
	// Config is merged into every kernel's configuration file.
	Config() map[string]any
	// Scale is how many base units make one credit.
	Scale() int64
	// GasUp gives a kernel gas for the payments it will make. A kernel funded for one when it owes
	// three stalls on the second, holding its reserve, which reads as a stuck rail.
	GasUp(k *Kernel, payments int) error
	// Fund puts money in a user's hands and returns when the kernel counts it as theirs.
	Fund(k *Kernel, user string, credits int64) error
	// Credit closes one obligation on the seller's books against the payment the buyer made for it.
	// On a world whose finalized facts are the operator's own records this is that record; on a
	// chain the kernel's own deposit scan does it, and this only reports whether it has yet.
	Credit(seller *Kernel, ticketID, buyer string, amount int64) error
	// SettleWait is how long a payment may take to become final on this rail, so the story waits for
	// what a chain actually needs rather than a figure guessed once.
	SettleWait() time.Duration
	// Finish measures what the run cost and cleans up.
	Finish(n *Net) (map[string]any, error)
}

// Shape is what the story will ask of the rail, declared before anything runs.
type Shape struct {
	Kernels        int
	PayingUsers    int // accounts funded from outside the economy
	SigningKernels int // kernels that will sign at least one payment
	Settlements    int // one per ordered pair that ends in debt, from every act
	// PaymentsPerKernel is how many payments each kernel makes; it must hold gas for all of them.
	PaymentsPerKernel map[string]int
}

func NewRail(name string) (Rail, error) {
	switch name {
	case "play":
		return &playRail{}, nil
	case "anvil":
		return &anvilRail{chainRail: chainRail{scale: 1_000_000, await: 3 * time.Minute}}, nil
	case "sepolia":
		return &sepoliaRail{chainRail: chainRail{scale: 1_000_000, await: 40 * time.Minute}}, nil
	}
	return nil, fmt.Errorf("unknown rail %q (want play, anvil or sepolia)", name)
}

// ---- play -------------------------------------------------------------------
// No chain: the operator's own record is the finalized fact, so a payment and its finality are the
// same act.

type playRail struct{}

func (playRail) Name() string                        { return "play" }
func (playRail) Prepare(n *Net, s Shape) error       { return nil }
func (playRail) Config() map[string]any              { return map[string]any{"world": "play", "rail_rpc": ""} }
func (playRail) Scale() int64                        { return 1 }
func (playRail) GasUp(k *Kernel, payments int) error { return nil }
func (playRail) Finish(n *Net) (map[string]any, error) {
	return map[string]any{"rail": "play", "cost": "none"}, nil
}

func (playRail) Fund(k *Kernel, user string, credits int64) error {
	ref := fmt.Sprintf("netsim-%s-%d", user, time.Now().UnixNano())
	_, err := k.Run("sysop-"+k.Name, "admin", "deposit", "--yes", user, strconv.FormatInt(credits, 10), "--ref", ref)
	return err
}

// Credit is the operator's own record that the buyer's payment arrived, which on this world is what
// makes it final.
func (playRail) Credit(seller *Kernel, ticketID, buyer string, amount int64) error {
	return confirmPayment(seller, ticketID, buyer, amount)
}

// SettleWait is nothing: the operator's record is the finality.
func (playRail) SettleWait() time.Duration { return 5 * time.Second }

// ---- a chain ----------------------------------------------------------------
// What anvil and Sepolia share: real contracts, real signatures, a payment that is final only when
// the chain says so. They differ in who pays for gas, how blocks advance, and how long finality
// takes, which is what the fields hold.

type chainRail struct {
	rpc, payer, token, worldFile string
	scale                        int64
	await                        time.Duration
	gas                          func(payments int) string
	wallet                       *payingWallet
	vaults                       []string // every kernel vault this run put gas into
}

func (c *chainRail) Config() map[string]any {
	return map[string]any{"world": c.worldFile, "rail_rpc": c.rpc}
}
func (c *chainRail) Scale() int64 { return c.scale }

func (c *chainRail) GasUp(k *Kernel, payments int) error {
	vault := k.Field("sysop-"+k.Name, "rail_address", "admin", "identity")
	if vault == "" {
		return fmt.Errorf("kernel %s serves no rail address", k.Name)
	}
	if _, err := cast("send", "--private-key", c.payer, "--rpc-url", c.rpc, vault, "--value", c.gas(payments)); err != nil {
		return err
	}
	c.vaults = append(c.vaults, vault)
	return nil
}

// Fund submits the payment: a wallet the user proves control of pays into the kernel's vault. It
// does not wait for the credit. The kernel counts the money only once the chain has finalized it,
// which on a public chain is a quarter of an hour, and waiting for each of seven payers in turn
// would be most of a morning. Every payment is submitted, then the story waits for all of them at
// once. The submissions are sequential because they share one wallet, whose transactions must be
// ordered.
func (c *chainRail) Fund(k *Kernel, user string, credits int64) error {
	base := credits * c.scale
	w := c.wallet
	if _, err := cast("send", "--private-key", c.payer, "--rpc-url", c.rpc, c.token,
		"mint(address,uint256)", w.addr, strconv.FormatInt(base, 10)); err != nil {
		return fmt.Errorf("minting %d for %s: %w", base, user, err)
	}
	vault := k.Field("sysop-"+k.Name, "rail_address", "admin", "identity")
	uid := k.Field(user, "id", "user", "me")
	msg := fmt.Sprintf("juice address registration\nkernel: %s\nuser: %s\naddress: %s", k.Key, uid, w.addr)
	sig := castOut("wallet", "sign", "--private-key", w.key, msg)
	_, _ = k.Run(user, "user", "address", w.addr, "--signature", sig)
	if _, err := cast("send", "--private-key", w.key, "--rpc-url", c.rpc, c.token,
		"transfer(address,uint256)", vault, strconv.FormatInt(base, 10)); err != nil {
		return fmt.Errorf("paying %d into %s: %w", base, k.Name, err)
	}
	return nil
}

// Credit does nothing on a chain: the seller's own deposit scan closes an obligation once the
// payment the buyer named is final, which is the property under test. Reporting success here would
// be the story doing the kernel's work for it.
func (c *chainRail) Credit(seller *Kernel, ticketID, buyer string, amount int64) error {
	return nil
}

// SettleWait is how long the chain takes to make a payment final, which is what the story waits for.
func (c *chainRail) SettleWait() time.Duration { return c.await }

// confirmPayment is the operator's own confirmation that a payment arrived, naming the obligation it
// closes. Flags first, then a bare `--`: a public key is base64url and may begin with a dash, which
// is otherwise read as an unknown flag and leaves the obligation silently open.
func confirmPayment(seller *Kernel, ticketID, buyer string, amount int64) error {
	_, err := seller.Run("sysop-"+seller.Name, "admin", "deposit", "--yes", "--ref", ticketID,
		"--", buyer, strconv.FormatInt(amount, 10))
	return err
}

// payingWallet is the one wallet the whole run pays in from. An address registers per kernel, so one
// wallet can pay into every kernel; a fresh wallet per user would cost a funding transaction each
// time, which on a rationed testnet is the difference between a gate that can run and one that
// cannot. It is funded for exactly what the shape says it will do — one registration and one token
// transfer per paying user — which is also what cost() prices.
type payingWallet struct{ key, addr string }

func newPayingWallet(n *Net, rpc, payer string, transfers int) (*payingWallet, error) {
	w := &payingWallet{key: newWallet()}
	n.Secret(w.key, "<key:paying-wallet>")
	w.addr = castOut("wallet", "address", "--private-key", w.key)
	need := sepTxFee + float64(transfers)*sepERC20Fee
	if _, err := cast("send", "--private-key", payer, "--rpc-url", rpc, w.addr,
		"--value", fmt.Sprintf("%.9fether", need)); err != nil {
		return nil, fmt.Errorf("funding the paying wallet: %w", err)
	}
	return w, nil
}

// ---- anvil ------------------------------------------------------------------
// A local chain: blocks are made on demand and the currency costs nothing, so this is where the
// expensive paths are exercised.

type anvilRail struct {
	chainRail
	proc   *exec.Cmd
	mining chan struct{}
}

func (anvilRail) Name() string { return "anvil" }

func (a *anvilRail) Prepare(n *Net, s Shape) error {
	for _, tool := range []string{"anvil", "cast"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("the anvil rail needs Foundry: %s is not on PATH", tool)
		}
	}
	contracts := os.Getenv("JUICE_RAIL_CONTRACTS")
	if contracts == "" {
		contracts, _ = filepath.Abs("../juice-rail/contracts/out")
	}
	if _, err := os.Stat(contracts); err != nil {
		return fmt.Errorf("cannot find the rail contracts at %s (set JUICE_RAIL_CONTRACTS)", contracts)
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	logf, err := os.Create(filepath.Join(n.Root, "anvil.log"))
	if err != nil {
		return err
	}
	a.proc = exec.Command("anvil", "--port", strconv.Itoa(port), "--chain-id", "31337", "--slots-in-an-epoch", "1", "--silent")
	a.proc.Stdout, a.proc.Stderr = logf, logf
	if err := a.proc.Start(); err != nil {
		return err
	}
	a.rpc = fmt.Sprintf("http://127.0.0.1:%d", port)
	if !poll(20*time.Second, 250*time.Millisecond, func() bool { return castOut("block-number", "--rpc-url", a.rpc) != "" }) {
		return fmt.Errorf("anvil did not start on %s", a.rpc)
	}
	// Anvil's first account: a published constant of the tool, not a secret.
	a.payer = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	a.gas = func(int) string { return "1ether" } // free, so only ample
	// A local chain makes blocks only when asked. Mining on a ticker for the life of the run means
	// nothing else has to know that: funding and settlement wait for their condition here exactly
	// as they do on a public chain.
	a.mining = make(chan struct{})
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-a.mining:
				return
			case <-t.C:
				_, _ = cast("rpc", "anvil_mine", "4", "--rpc-url", a.rpc)
			}
		}
	}()

	token, err := a.deploy(contracts, "MockUSDT0", "", "")
	if err != nil {
		return fmt.Errorf("deploying the token: %w", err)
	}
	weth := "0x000000000000000000000000000000000000eEEE"
	args := castOut("abi-encode", "f(address,address,uint24,uint256)", token, weth, "500", "3000000000")
	router, err := a.deploy(contracts, "MockRouter", "10ether", args)
	if err != nil {
		return fmt.Errorf("deploying the router: %w", err)
	}
	a.token = token
	world := map[string]any{
		"name": "netsim-anvil", "chainId": 31337, "token": token, "decimals": 6,
		"finality": "finalized", "fromBlock": 0,
		// The largest ticket a kernel on this world may write, like the shipped worlds carry: the
		// story draws at storyLottery tokens, and a world without a ceiling permits no ticket at all.
		"lotteryMax": storyLottery * a.scale,
		"venue":      map[string]any{"router": router, "quoter": router, "weth": weth, "feeTier": 500},
		"gas": map[string]any{"min": "20000000000000000", "max": "50000000000000000",
			"feeBound": "10000000000000000", "slippageBps": 50, "paymentGas": 300000, "swapGas": 1500000},
	}
	if a.worldFile, err = writeWorld(n, world); err != nil {
		return err
	}
	if a.wallet, err = newPayingWallet(n, a.rpc, a.payer, s.PayingUsers); err != nil {
		return err
	}
	fmt.Printf("  rail: anvil — chain %s, token %s\n", a.rpc, token)
	return nil
}

func (a *anvilRail) deploy(dir, name, value, ctorArgs string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name+".sol", name+".json"))
	if err != nil {
		return "", err
	}
	var art struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(raw, &art); err != nil {
		return "", err
	}
	args := []string{"send", "--private-key", a.payer, "--rpc-url", a.rpc}
	if value != "" {
		args = append(args, "--value", value)
	}
	out, err := cast(append(args, "--create", art.Bytecode.Object+strings.TrimPrefix(ctorArgs, "0x"), "--json")...)
	if err != nil {
		return "", err
	}
	var r struct {
		ContractAddress string `json:"contractAddress"`
	}
	if json.Unmarshal([]byte(out), &r) != nil || r.ContractAddress == "" {
		return "", fmt.Errorf("no contract address in the deployment reply")
	}
	return r.ContractAddress, nil
}

func (a *anvilRail) Finish(n *Net) (map[string]any, error) {
	if a.mining != nil {
		close(a.mining)
	}
	if a.proc != nil && a.proc.Process != nil {
		_ = a.proc.Process.Kill()
	}
	return map[string]any{"rail": "anvil", "chain": a.rpc, "cost": "none, the chain is local"}, nil
}

// ---- sepolia ----------------------------------------------------------------
// Arbitrum Sepolia: the contracts and finality a deployment would meet, and a currency that, while
// worthless, is rationed. The run prices itself before spending and cannot exceed what it moved into
// its own spending wallet.

type sepoliaRail struct {
	chainRail
	keyfile, spenderAddr, funderAddr string
	cap                              float64
}

// Measured on Arbitrum Sepolia, 2026-09-04 at 0.48 gwei.
const (
	sepTxFee      = 0.000025 // one plain ETH send
	sepERC20Fee   = 0.00013  // one token transfer
	sepMintFee    = 0.00005  // one mock-token mint
	sepPaymentGas = 0.000144 // the world's paymentGas at that price
	sepReserve    = 0.00002  // the floor a kernel refuses to dip below
)

func (sepoliaRail) Name() string { return "sepolia" }

func (s *sepoliaRail) Prepare(n *Net, shape Shape) error {
	s.rpc, s.keyfile = os.Getenv("JUICE_SEPOLIA_RPC"), os.Getenv("JUICE_SEPOLIA_KEY_FILE")
	if s.rpc == "" || s.keyfile == "" {
		return fmt.Errorf("the sepolia rail needs JUICE_SEPOLIA_RPC and JUICE_SEPOLIA_KEY_FILE")
	}
	// An allowlist: naming the chains to avoid would leave every other real network one mistyped
	// RPC away from a run that spends real money.
	if chainID := castOut("chain-id", "--rpc-url", s.rpc); chainID != "421614" {
		return fmt.Errorf("refusing: that RPC reports chain %q; this rail runs on Arbitrum Sepolia (421614) and nowhere else", chainID)
	}
	fi, err := os.Stat(s.keyfile)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s must be mode 600; it is %o", s.keyfile, fi.Mode().Perm())
	}
	// Priced before anything is spent. If it does not fit, the answer is to say so, never to run a
	// smaller economy under the same name.
	want, cap := s.cost(shape), envFloat("JUICE_SEPOLIA_BUDGET", 0.005)
	fmt.Printf("  budget: the story needs about %.6f ETH; the cap is %.6f\n", want, cap)
	if want > cap {
		return fmt.Errorf("the canonical story does not fit this rail's budget: it needs about %.6f ETH "+
			"(%d settlements, %d paying users) and the cap is %.6f. Raise JUICE_SEPOLIA_BUDGET "+
			"deliberately or run it on anvil; the story is not reduced to fit", want, shape.Settlements, shape.PayingUsers, cap)
	}
	s.cap = cap
	head, err := strconv.ParseInt(castOut("block", "finalized", "--rpc-url", s.rpc, "-f", "number"), 10, 64)
	if err != nil {
		return fmt.Errorf("no finalized head from %s", s.rpc)
	}
	raw, err := os.ReadFile("rail/worlds/test.json")
	if err != nil {
		return err
	}
	var w map[string]any
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	// Only {name, chainId, token} fix the network digest. fromBlock near the head spares the scanner
	// millions of blocks; the shipped gas band suits a kernel running for months, and one that lives
	// for a run needs only enough for its payments — which also keeps it off the refill path this
	// chain's shallow pool cannot serve (refill is exercised on anvil).
	w["fromBlock"] = head - 200
	if gas, ok := w["gas"].(map[string]any); ok {
		gas["min"], gas["max"], gas["feeBound"] = "20000000000000", "60000000000000", "20000000000000"
	}
	s.token, _ = w["token"].(string)
	if s.worldFile, err = writeWorld(n, w); err != nil {
		return err
	}
	// An estimate is not a cap. The run never spends from the funder: exactly the cap is moved
	// into a wallet made for this run, and whatever the estimate got wrong, the run cannot exceed
	// what that wallet holds.
	funder, err := os.ReadFile(s.keyfile)
	if err != nil {
		return err
	}
	fkey := strings.TrimSpace(string(funder))
	n.Secret(fkey, "<key:funder>")
	s.funderAddr = castOut("wallet", "address", "--private-key", fkey)
	if have := s.balance(s.funderAddr); have < cap {
		return fmt.Errorf("the funding account holds %.6f ETH and the cap is %.6f", have, cap)
	}
	s.payer = newWallet()
	n.Secret(s.payer, "<key:spender>")
	s.spenderAddr = castOut("wallet", "address", "--private-key", s.payer)
	if _, err := cast("send", "--private-key", fkey, "--rpc-url", s.rpc, s.spenderAddr, "--value", fmt.Sprintf("%.9fether", cap)); err != nil {
		return fmt.Errorf("funding the spending wallet: %w", err)
	}
	fmt.Printf("  spending wallet %s holds exactly %.6f ETH; the run cannot exceed it\n", s.spenderAddr, cap)
	s.gas = func(payments int) string {
		return fmt.Sprintf("%.9fether", float64(payments)*sepPaymentGas+sepReserve+sepTxFee)
	}
	s.wallet, err = newPayingWallet(n, s.rpc, s.payer, shape.PayingUsers)
	return err
}

// cost is every blockchain transaction the declared story will cause, by who signs it: the spender
// mints for each paying user and sends gas to the wallet and each settling kernel; the wallet
// registers once and transfers once per paying user; each kernel holds gas for every payment it
// makes plus its reserve and a send of margin.
func (s *sepoliaRail) cost(sh Shape) float64 {
	spender := float64(sh.PayingUsers)*sepMintFee + float64(1+len(sh.PaymentsPerKernel))*sepTxFee
	wallet := sepTxFee + float64(sh.PayingUsers)*sepERC20Fee
	kernels := 0.0
	for _, payments := range sh.PaymentsPerKernel {
		kernels += float64(payments)*sepPaymentGas + sepReserve + sepTxFee
	}
	return spender + wallet + kernels
}

func (s *sepoliaRail) balance(addr string) float64 {
	if addr == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(castOut("balance", addr, "--rpc-url", s.rpc, "--ether"), 64)
	return f
}

// Finish separates what was burned from what merely moved: ETH in a kernel's vault or the paying
// wallet has left the funder but not been spent. The spender's remainder goes back to the funder
// when it is worth a send; a vault can only be moved by its kernel, so those are listed.
func (s *sepoliaRail) Finish(n *Net) (map[string]any, error) {
	left := s.balance(s.spenderAddr)
	parked := 0.0
	if s.wallet != nil {
		parked += s.balance(s.wallet.addr)
	}
	for _, v := range s.vaults {
		parked += s.balance(v)
	}
	swept := 0.0
	if left > 3*sepTxFee && s.funderAddr != "" {
		swept = left - 2*sepTxFee
		if _, err := cast("send", "--private-key", s.payer, "--rpc-url", s.rpc, s.funderAddr, "--value", fmt.Sprintf("%.9fether", swept)); err != nil {
			swept = 0
		}
	}
	burned := s.cap - left - parked
	fmt.Printf("  burned %.6f ETH of the %.6f cap; %.6f parked in vaults and the paying wallet; %.6f returned\n", burned, s.cap, parked, swept)
	m := map[string]any{"rail": "sepolia", "cap_eth": s.cap, "burned_eth": burned, "parked_eth": parked,
		"parked_in_vaults": s.vaults, "swept_back_eth": swept, "exhausted": left <= sepReserve}
	if left <= sepReserve {
		return m, fmt.Errorf("the spending wallet ran dry: the run could not pay for all its work")
	}
	return m, nil
}

// ---- shared -----------------------------------------------------------------

// writeWorld records the world file and the digest that names the network a chain rail talked to.
// Only {name, chainId, token} define it, so it is stable across runs that tune gas or the scan
// start and different across runs that do not share a network.
func writeWorld(n *Net, w map[string]any) (string, error) {
	n.Run.WorldDigest = fmt.Sprintf("%v/%v/%v", w["name"], w["chainId"], w["token"])
	b, _ := json.MarshalIndent(w, "", " ")
	path := filepath.Join(n.Root, "world.json")
	return path, os.WriteFile(path, b, 0o644)
}

// cast runs Foundry's tool and keeps its error. Turning a failure into an empty string makes a
// failed deployment look like an empty address and an unsent transaction look sent.
func cast(args ...string) (string, error) {
	out, err := exec.Command("cast", args...).Output()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("cast %s: %v: %s", args[0], err, firstLine(msg))
	}
	return strings.TrimSpace(string(out)), nil
}

// castOut is for reads whose failure the caller handles by inspecting the value.
func castOut(args ...string) string { out, _ := cast(args...); return out }

func newWallet() string {
	var w []struct {
		PrivateKey string `json:"private_key"`
	}
	if json.Unmarshal([]byte(castOut("wallet", "new", "--json")), &w) != nil || len(w) == 0 {
		return ""
	}
	return w[0].PrivateKey
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func envFloat(name string, def float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
		return f
	}
	return def
}
