// SPDX-License-Identifier: AGPL-3.0-only

// Package rail is the boundary between the kernel's ledger and real money (D23). It owns the world
// files that define a network, and the adaptors that witness external payments: the manual one,
// whose finalized facts are the operator's own records, and the chain one over juice-rail. It is the
// only package that imports a chain library, so the kernel never does.
package rail

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	jrail "github.com/daios-ai/juice-rail/go/rail"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/daios-ai/juice/kernel"
)

// The worlds this build ships. They are carried only to be written into the installation's own
// worlds directory the first time it is served: from then on the files on disk are the worlds, so
// an operator who edits an endpoint or a seed list keeps that edit across an upgrade, and adds a
// network by adding a file. A world file is the juice-rail domain document plus its seeds: the
// operator may write their own and get an isolated economy, isolated rather than private, since
// anyone holding the file can join.
//
//go:embed worlds
var shipped embed.FS

// The adaptors a world may name. The field decides which one witnesses this network's money, so a
// world says what it settles on rather than leaving it to be inferred from which fields it happens
// to carry — and a future adaptor is a new value here, not a new guess.
const (
	RailManual = "manual"
	RailEVM    = "evm"
)

// World is one network's definition. The defining part — name, rail, chain, token — is identical
// for every member and fixes the network fingerprint; everything else is operational and belongs to
// whoever runs the kernel, so changing an endpoint or a gas policy never changes the network.
type World struct {
	// Name is the file's own name, never a field inside it: one string names the world on the
	// command line, the directory the kernel lives in, and the network itself, so the three
	// cannot disagree.
	Name     string `json:"-"`
	Rail     string `json:"rail"`
	ChainID  uint64 `json:"chainId"`
	Token    string `json:"token"`
	Decimals uint8  `json:"decimals"`
	// Symbol is what an amount on this world is called when it is shown to a person, and Description
	// is the one line that tells an operator choosing a network what this one means. Both are display
	// only: neither enters the fingerprint, so renaming a token or rewording a line is not a new network.
	Symbol      string `json:"symbol"`
	Description string `json:"description"`

	// Seeds are the bootstrap addresses of this network's own kernels — the meeting point a new
	// member dials before it knows anyone. Each world has its own, since a kernel that dialled
	// another world's seed would be told, every pass, that it serves a network this one is not.
	// An empty list is a network whose members introduce each other by editing this file.
	Seeds []string `json:"seeds"`

	// RPC is where this kernel reaches its chain. A chain world must name one; an operator who
	// wants their own node edits it here, beside the chain it belongs to. There is no field for
	// where the payment scan starts: that is not the network's to say and not the operator's
	// either — it is the block the chain reports when a kernel first reaches it, recorded then as
	// the rail's own cursor.
	RPC      string   `json:"rpc"`
	Finality string   `json:"finality"`
	Venue    venueCfg `json:"venue"`
	Gas      gasCfg   `json:"gas"`
}

type venueCfg struct {
	Router        string `json:"router"`
	Quoter        string `json:"quoter"`
	WrappedNative string `json:"wrappedNative"`
	FeeTier       uint32 `json:"feeTier"`
	Router02      bool   `json:"router02"`
}

type gasCfg struct {
	Min         string `json:"min"`
	Max         string `json:"max"`
	FeeBound    string `json:"feeBound"`
	SlippageBps uint32 `json:"slippageBps"`
	PaymentGas  uint64 `json:"paymentGas"`
	SwapGas     uint64 `json:"swapGas"`
}

// Install writes every shipped world into dir that is not there already, and leaves the rest alone:
// a file an operator has edited is the world they serve, and an upgrade must not undo it. Each is
// written under a temporary name, flushed, and linked into place, so a crash cannot leave a
// half-written file under a name that would then never be rewritten; a link refused because the
// file now exists is another `serve` having won the race, which is success.
func Install(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := fs.ReadDir(shipped, "worlds")
	if err != nil {
		return err
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if _, err := os.Stat(path); err == nil {
			continue
		}
		body, err := fs.ReadFile(shipped, "worlds/"+e.Name())
		if err != nil {
			return err
		}
		// A name of this attempt's own: two kernels of one installation may be started at once, and
		// a shared temporary name would have each truncating and removing the other's file.
		tmp, err := os.CreateTemp(dir, e.Name()+".*")
		if err != nil {
			return err
		}
		tmp.Close()
		if err := writeSynced(tmp.Name(), string(body)); err != nil {
			os.Remove(tmp.Name())
			return err
		}
		lerr := os.Link(tmp.Name(), path)
		os.Remove(tmp.Name())
		if lerr != nil && !os.IsExist(lerr) {
			return lerr
		}
	}
	return syncDir(dir)
}

// Load reads the world named name from dir. The name is the file's, minus `.json`; a world that is
// not there is named as the path it would be, since that is the file to write or to correct.
func Load(dir, name string) (World, error) {
	// Before anything is read: a name is one file in dir, never a path through it, so no caller can
	// reach outside the directory of worlds by naming one.
	if err := validName(name); err != nil {
		return World{}, err
	}
	path := filepath.Join(dir, name+".json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return World{}, fmt.Errorf("there is no world called %s: %s does not exist, and a world is a file in that directory", name, path)
	}
	if err != nil {
		return World{}, err
	}
	// Strict, as config.json is: a key this build does not know is a setting the operator meant to
	// have an effect and that would silently have none, so the refusal names it.
	var w World
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return World{}, fmt.Errorf("parse world file: %w", err)
	}
	// One document and nothing after it: a second object in the file is a world somebody meant to
	// serve, silently ignored.
	if dec.More() {
		return World{}, fmt.Errorf("parse world file: more than one document")
	}
	w.Name = name
	if err := w.validate(); err != nil {
		return World{}, err
	}
	return w, nil
}

// validate rejects a world nobody could serve. A chain world must name every field the rail needs
// before any money depends on it.
func (w World) validate() error {
	if err := validName(w.Name); err != nil {
		return err
	}
	switch w.Rail {
	case RailManual:
		if w.ChainID != 0 || w.Token != "" {
			return fmt.Errorf("world %q settles on no chain, so it names neither chainId nor token", w.Name)
		}
		return nil
	case RailEVM:
		if _, err := jrail.ParseAddress(w.Token, "token"); err != nil {
			return err
		}
		if w.ChainID == 0 {
			return fmt.Errorf("world %q must name the chainId its token is on", w.Name)
		}
		if w.RPC == "" {
			return fmt.Errorf("world %q must name the node it reaches its chain through, in %q", w.Name, "rpc")
		}
		// The ledger holds 64-bit integers, so a token whose unit needs more than 18 decimals could
		// not have even ten of itself represented; such a world is refused rather than silently wrapped.
		if w.Decimals == 0 || w.Decimals > 18 {
			return fmt.Errorf("world %q must state the token's decimals, at most 18", w.Name)
		}
		return nil
	default:
		return fmt.Errorf("world %q names rail %q; this build settles on %q or %q", w.Name, w.Rail, RailManual, RailEVM)
	}
}

// validName accepts what may name a world: one plain name, since it is at once a file in the worlds
// directory, the directory its kernel lives in, and the network itself.
func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\@ `) {
		return fmt.Errorf("world name %q must be a bare name", name)
	}
	return nil
}

// definingPart is exactly what every member of a network shares, and nothing else. Its canonical
// form is hashed into the fingerprint, so two kernels agree iff they run the same network — whatever
// endpoint, gas policy, or file name each of them uses locally. The adaptor is in it because two
// worlds settling by different rules are different money even where everything else matches.
type definingPart struct {
	ChainID uint64 `json:"chain_id"`
	Name    string `json:"name"`
	Rail    string `json:"rail"`
	Token   string `json:"token"`
}

// Network is what the kernel needs: the name for people, the fingerprint for signatures and discovery,
// the decimals a client renders amounts with, and the token those amounts are paid in — empty
// where the world has no chain, and the only thing that says which money this is.
func (w World) Network() kernel.Network {
	token := ""
	if w.Token != "" {
		token = strings.ToLower(common.HexToAddress(w.Token).Hex())
	}
	canon, err := kernel.CanonicalJSON(definingPart{ChainID: w.ChainID, Name: w.Name, Rail: w.Rail, Token: token})
	if err != nil {
		// definingPart is four scalars; it cannot fail to canonicalize.
		panic("rail: canonicalize world: " + err.Error())
	}
	sum := sha256.Sum256(canon)
	return kernel.Network{Name: w.Name, Fingerprint: hex.EncodeToString(sum[:]), Decimals: w.Decimals,
		Symbol: w.Symbol, Token: token}
}

// Domain converts the world into the rail library's domain. Only a chain world has one.
func (w World) Domain() (jrail.Domain, error) {
	if w.Rail != RailEVM {
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
	weth, err := jrail.ParseAddress(w.Venue.WrappedNative, "venue wrappedNative")
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
		// FromBlock is left at zero: the rail reads it only when its own cursor is absent, and
		// OpenChain seeds that cursor before any scan can run, so it is never consulted.
		Decimals: w.Decimals, Finality: finality,
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
