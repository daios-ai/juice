// Package rail is the boundary between the kernel's ledger and real money (D23). It owns the world
// files that define a network, and the adaptors that witness external payments: the manual one,
// whose finalized facts are the operator's own records, and the chain one over juice-rail. It is the
// only package that imports a chain library, so the kernel never does.
package rail

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"

	jrail "github.com/daios-ai/juice-rail/go/rail"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/daios-ai/juice/kernel"
)

// Shipped worlds. A world file is the juice-rail domain document plus a name: the operator may write
// their own and get an isolated economy, isolated rather than private, since anyone holding the file
// can join.
var (
	//go:embed worlds/play.json
	worldPlay []byte
	//go:embed worlds/test.json
	worldTest []byte
	//go:embed worlds/real.json
	worldReal []byte
)

// World is one network's definition. The defining part — name, chain, token — is identical for every
// member and fixes the network digest; everything else is operational and belongs to whoever runs
// the kernel, so changing an endpoint or a gas policy never changes the network.
type World struct {
	Name     string `json:"name"`
	ChainID  uint64 `json:"chainId"`
	Token    string `json:"token"`
	Decimals uint8  `json:"decimals"`

	RPC       string   `json:"rpc"`
	Finality  string   `json:"finality"`
	FromBlock uint64   `json:"fromBlock"`
	Venue     venueCfg `json:"venue"`
	Gas       gasCfg   `json:"gas"`
	// LotteryMax is the largest ticket a kernel on this world may write (P10). It rides with the
	// rail because it is a property of what a payment there costs, and it is outside the defining
	// part so it can follow that cost without splitting the network. An operator picks its own
	// lottery at or below it; every kernel refuses a foreign call quoting more.
	LotteryMax int64 `json:"lotteryMax"`
}

type venueCfg struct {
	Router   string `json:"router"`
	Quoter   string `json:"quoter"`
	WETH     string `json:"weth"`
	FeeTier  uint32 `json:"feeTier"`
	Router02 bool   `json:"router02"`
}

type gasCfg struct {
	Min         string `json:"min"`
	Max         string `json:"max"`
	FeeBound    string `json:"feeBound"`
	SlippageBps uint32 `json:"slippageBps"`
	PaymentGas  uint64 `json:"paymentGas"`
	SwapGas     uint64 `json:"swapGas"`
}

// Load resolves a shipped world by name, or reads one from a file path. An unknown bare name is an
// error rather than a path attempt, so a typo never silently becomes "file not found".
func Load(nameOrPath string) (World, error) {
	var raw []byte
	switch nameOrPath {
	case "", "play":
		raw = worldPlay
	case "test":
		raw = worldTest
	case "real":
		raw = worldReal
	default:
		if !strings.ContainsAny(nameOrPath, "/.") {
			return World{}, fmt.Errorf("unknown world %q: use play, test, real, or the path to a world file", nameOrPath)
		}
		b, err := os.ReadFile(nameOrPath)
		if err != nil {
			return World{}, fmt.Errorf("read world file: %w", err)
		}
		raw = b
	}
	var w World
	if err := json.Unmarshal(raw, &w); err != nil {
		return World{}, fmt.Errorf("parse world file: %w", err)
	}
	if err := w.validate(); err != nil {
		return World{}, err
	}
	return w, nil
}

// validate rejects a world nobody could serve. A chain world must name every field the rail needs
// before any money depends on it.
func (w World) validate() error {
	if w.Name == "" {
		return fmt.Errorf("world file has no name")
	}
	if w.LotteryMax < 0 {
		return fmt.Errorf("world %q states a negative lottery ceiling", w.Name)
	}
	if strings.ContainsAny(w.Name, "/@ ") {
		return fmt.Errorf("world name %q must be a bare name", w.Name)
	}
	if !w.Chained() {
		if w.ChainID != 0 || w.Token != "" {
			return fmt.Errorf("world %q names only one of chainId and token; a chain world needs both", w.Name)
		}
		return nil
	}
	if _, err := jrail.ParseAddress(w.Token, "token"); err != nil {
		return err
	}
	// The ledger holds 64-bit integers, so a token whose unit needs more than 18 decimals could not
	// have even ten of itself represented; such a world is refused rather than silently wrapped.
	if w.Decimals == 0 || w.Decimals > 18 {
		return fmt.Errorf("world %q must state the token's decimals, at most 18", w.Name)
	}
	return nil
}

// Chained reports whether this world settles on a chain. It reads the defining part alone, which is
// what selects the adaptor: no code anywhere asks whether the world is called play or real.
func (w World) Chained() bool { return w.ChainID != 0 && w.Token != "" }

// definingPart is exactly what every member of a network shares, and nothing else. Its canonical
// form is hashed into the digest, so two kernels agree iff they run the same network — whatever
// endpoint, gas policy, or file name each of them uses locally.
type definingPart struct {
	ChainID uint64 `json:"chain_id"`
	Name    string `json:"name"`
	Token   string `json:"token"`
}

// Network is what the kernel needs: the name for people, the digest for signatures and discovery,
// and the decimals a client renders amounts with.
func (w World) Network() kernel.Network {
	token := ""
	if w.Token != "" {
		token = strings.ToLower(common.HexToAddress(w.Token).Hex())
	}
	canon, err := kernel.CanonicalJSON(definingPart{ChainID: w.ChainID, Name: w.Name, Token: token})
	if err != nil {
		// definingPart is three scalars; it cannot fail to canonicalize.
		panic("rail: canonicalize world: " + err.Error())
	}
	sum := sha256.Sum256(canon)
	return kernel.Network{Name: w.Name, Digest: hex.EncodeToString(sum[:]), Decimals: w.Decimals}
}

// Domain converts the world into the rail library's domain. Only a chain world has one.
func (w World) Domain() (jrail.Domain, error) {
	if !w.Chained() {
		return jrail.Domain{}, fmt.Errorf("world %q has no chain", w.Name)
	}
	token, err := jrail.ParseAddress(w.Token, "token")
	if err != nil {
		return jrail.Domain{}, err
	}
	router, err := jrail.ParseAddress(w.Venue.Router, "venue router")
	if err != nil {
		return jrail.Domain{}, err
	}
	quoter, err := jrail.ParseAddress(w.Venue.Quoter, "venue quoter")
	if err != nil {
		return jrail.Domain{}, err
	}
	weth, err := jrail.ParseAddress(w.Venue.WETH, "venue weth")
	if err != nil {
		return jrail.Domain{}, err
	}
	gas, err := w.Gas.parse()
	if err != nil {
		return jrail.Domain{}, err
	}
	// The settlement tag is the world's choice: latest, safe or finalized. Below true finality a
	// settled fact is the sequencer's word, and the kernel prices that risk through the remote
	// premium; the rail does not. Absent, the strictest applies.
	tag := w.Finality
	if tag == "" {
		tag = "finalized"
	}
	var finality rpc.BlockNumber
	if err := finality.UnmarshalJSON([]byte(strconv.Quote(tag))); err != nil {
		return jrail.Domain{}, fmt.Errorf("world %q: finality %q: %w", w.Name, tag, err)
	}
	d := jrail.Domain{
		Name: w.Name, ChainID: new(big.Int).SetUint64(w.ChainID), Token: token,
		Decimals: w.Decimals, Finality: finality, FromBlock: w.FromBlock,
		Venue: jrail.Venue{Router: router, Quoter: quoter, WETH: weth,
			FeeTier: w.Venue.FeeTier, Router02: w.Venue.Router02},
		Gas: gas,
	}
	return d, d.Validate()
}

// parse turns the wei strings into integers. They are strings in the file because JSON numbers
// cannot carry a wei value without losing precision.
func (g gasCfg) parse() (jrail.GasPolicy, error) {
	num := func(s, what string) (*big.Int, error) {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return nil, fmt.Errorf("gas %s %q is not a whole number of wei", what, s)
		}
		return v, nil
	}
	min, err := num(g.Min, "min")
	if err != nil {
		return jrail.GasPolicy{}, err
	}
	max, err := num(g.Max, "max")
	if err != nil {
		return jrail.GasPolicy{}, err
	}
	bound, err := num(g.FeeBound, "feeBound")
	if err != nil {
		return jrail.GasPolicy{}, err
	}
	return jrail.GasPolicy{Min: min, Max: max, FeeBound: bound,
		SlippageBps: g.SlippageBps, PaymentGas: g.PaymentGas, SwapGas: g.SwapGas}, nil
}
