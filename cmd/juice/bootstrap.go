// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/native"
	"github.com/daios-ai/juice/rail"
	"github.com/daios-ai/juice/store"
)

const (
	// configKeyWorldFingerprint records the network this database belongs to, written once and checked
	// at every startup (D9, D23).
	configKeyWorldFingerprint = "world_fingerprint"
	configKeySuperuser        = "superuser_handle"
	configKeySigningPublic    = "signing_public_key"
	superuserHandle           = "sys"
)

// firstBootConfig gathers the configuration of a kernel that does not exist yet. It asks and
// nothing more: the caller writes, under the home's lock, which is the only write of config.json
// there is — every later boot reads it and leaves it alone, so a runtime-only override
// (JUICE_CREDENTIALS_KEY) never reaches the disk. What the operator pre-seeded stands; the rest is
// asked, because these are the facts a kernel cannot revise.
func firstBootConfig(w rail.World, home string) (ServerConfig, error) {
	path := filepath.Join(home, "config.json")
	cfg, err := LoadConfig(path)
	if err != nil && !os.IsNotExist(err) {
		return cfg, err
	}
	// A first boot has no file to override, so what the command line says is simply what the
	// kernel is: it answers the questions below rather than being asked them, and is written out
	// as the kernel's own configuration (§14).
	applyConfigFlags(&cfg)
	// A configuration written in advance is the operator saying that this kernel should exist —
	// in the file, or on the command line, which says the same things. Without either, they are
	// asked, and told what is already here, since a world that is not on that list is usually a
	// name mistyped.
	if os.IsNotExist(err) && !configFlagsGiven() {
		here := "No kernels here yet."
		if others := kernelsHere(); len(others) > 0 {
			here = "Kernels here: " + strings.Join(others, ", ") + "."
		}
		if !interactiveTTY() {
			return cfg, fmt.Errorf("there is no kernel on %s here, and no terminal to ask. %s\n"+
				"       To create it without a terminal, run this again with --kernel-handle NAME,\n"+
				"       or write %s to %s",
				w.Name, here, `"kernel_handle": "NAME"`, path)
		}
		fmt.Fprintf(os.Stderr, "There is no kernel on %s here. %s\n", w.Name, here)
		fmt.Fprintf(os.Stderr, "%s is %s.\n", w.Name, w.Description)
		if aerr := askYesNo(fmt.Sprintf("Create a kernel on %s?", w.Name)); aerr != nil {
			return cfg, aerr
		}
	}
	if cfg.KernelHandle == "" {
		h, aerr := askHandle(path)
		if aerr != nil {
			return cfg, aerr
		}
		cfg.KernelHandle = h
	}
	// Whatever answered — the file, the command line, the prompt — the name is held to the rule
	// peers apply to it, so no kernel can take one the network would refuse to hear.
	if verr := kernel.ValidateKernelName(cfg.KernelHandle); verr != nil {
		return cfg, fmt.Errorf("%s %q: %w", "kernel_handle", cfg.KernelHandle, verr)
	}
	if cfg.CredentialsKey == "" {
		raw := make([]byte, 32)
		if _, rerr := rand.Read(raw); rerr != nil {
			return cfg, fmt.Errorf("generate credentials key: %w", rerr)
		}
		cfg.CredentialsKey = base64.RawURLEncoding.EncodeToString(raw)
	}
	return cfg, nil
}

// askHandle asks what this kernel calls itself on the network, reading a whole line so an answer
// with a space reaches the validator that refuses it. Every kernel on one network shares the
// world's name, so this is the name that tells them apart, and the rule is the one peers apply to
// it when they hear it, never a looser local one. Off a terminal it refuses, naming the key and
// the file that would have supplied it.
func askHandle(path string) (string, error) {
	if !interactiveTTY() {
		return "", fmt.Errorf("no %q in %s, and no terminal to ask.\n"+
			"       Run this again with --kernel-handle NAME, or write it to that file", "kernel_handle", path)
	}
	fmt.Fprintf(os.Stderr, "\nWhat will this kernel call itself on the network? Other operators see this name.\n")
	for {
		fmt.Fprintf(os.Stderr, "Name: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		h := strings.TrimSpace(line)
		if h == "" {
			if err != nil {
				return "", fmt.Errorf("%s is required", "kernel_handle")
			}
			continue
		}
		if verr := kernel.ValidateKernelName(h); verr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", verr)
			continue
		}
		return h, nil
	}
}

// checkNetwork refuses a database that was made on another network. A kernel serves one network
// for life (D23): its balances, receipts and debts mean one thing only. Nothing maps a network
// back to a name — the name is the file that produced it — so the refusal reports both
// fingerprints and the world it was asked to serve.
func checkNetwork(ctx context.Context, db *store.DB, w rail.World) error {
	// A database that cannot be read must not read as one that was never bound: that is the one
	// answer that would serve a kernel's ledger under a second meaning.
	stored, err := db.GetConfig(ctx, configKeyWorldFingerprint)
	if err != nil && !errors.Is(err, kernel.ErrNotFound) {
		return err
	}
	if stored != "" && stored != w.Network().Fingerprint {
		return kernel.ErrInvalidState.Wrapf(
			"this kernel is bound to network %s, and %s is network %s; one kernel serves one network "+
				"for life, so serve it as the world it was created on or create a new kernel",
			stored, w.Name, w.Network().Fingerprint)
	}
	return nil
}

// bootCredentials are what a kernel is created with: the superuser's password, and the recovery
// phrase everything else comes from — the kernel's one key (P1) and the public half of the
// superuser's own recovery key (D9).
type bootCredentials struct {
	password    string
	key         *ecdsa.PrivateKey
	recoveryKey string
}

const (
	envBootstrapPassword = "JUICE_BOOTSTRAP_PASSWORD"
	envBootstrapPhrase   = "JUICE_BOOTSTRAP_PHRASE"
)

// kernelKeyPath is the one place a kernel's key is kept (D9).
func kernelKeyPath() string { return filepath.Join(kernelHome(), rail.KeyFile) }

// deriveKernelKey turns a recovery phrase into the kernel's one key: the BIP-39 seed, no
// passphrase, walked down the BIP-32 path m/44'/60'/0'/0/0 — the first account of an Ethereum
// wallet, so a compatible wallet given the same phrase opens the same address. The path is the
// protocol's and never changes: a phrase must mean one kernel forever (P1).
func deriveKernelKey(mnemonic string) (*ecdsa.PrivateKey, error) {
	seed, err := phraseSeed(mnemonic)
	if err != nil {
		return nil, err
	}
	const hardened = 0x80000000
	order := ethcrypto.S256().Params().N
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(seed)
	node := mac.Sum(nil)
	key, chain := new(big.Int).SetBytes(node[:32]), node[32:]
	for _, index := range []uint32{44 + hardened, 60 + hardened, hardened, 0, 0} {
		if key.Sign() == 0 || key.Cmp(order) >= 0 {
			return nil, kernel.ErrInvalidInput.Wrap("this recovery phrase yields no key; use another")
		}
		raw := key.FillBytes(make([]byte, 32))
		var data []byte
		if index >= hardened {
			data = append([]byte{0}, raw...)
		} else {
			parent, err := ethcrypto.ToECDSA(raw)
			if err != nil {
				return nil, kernel.ErrInternal.Wrapf("derive key: %v", err)
			}
			data = ethcrypto.CompressPubkey(&parent.PublicKey)
		}
		data = binary.BigEndian.AppendUint32(data, index)
		mac = hmac.New(sha512.New, chain)
		mac.Write(data)
		node = mac.Sum(nil)
		tweak := new(big.Int).SetBytes(node[:32])
		if tweak.Cmp(order) >= 0 {
			return nil, kernel.ErrInvalidInput.Wrap("this recovery phrase yields no key; use another")
		}
		key, chain = tweak.Add(tweak, key).Mod(tweak, order), node[32:]
	}
	if key.Sign() == 0 {
		return nil, kernel.ErrInvalidInput.Wrap("this recovery phrase yields no key; use another")
	}
	return ethcrypto.ToECDSA(key.FillBytes(make([]byte, 32)))
}

// firstBootCredentials gathers what a kernel is created with and checks it by the rule every
// account is created under, writing nothing, so a boot refused here leaves nothing behind. Each
// has one source off a terminal, its variable, and a refusal names that variable; at a terminal
// each is asked.
//
// The phrase is the kernel: a phrase given restores the kernel it belongs to, and none, at a
// terminal, creates a new one, whose phrase is shown once and acknowledged before anything is
// written. Off a terminal a phrase is never made up, because the only place it could be shown is a
// log. A home that already holds a key — a boot that stopped before it finished — takes only the
// phrase that key came from: the key alone cannot rebuild the superuser's recovery key.
func firstBootCredentials(w rail.World) (bootCredentials, error) {
	c := bootCredentials{password: os.Getenv(envBootstrapPassword)}
	source := envBootstrapPassword
	if c.password == "" {
		if !interactiveTTY() {
			return c, fmt.Errorf("no %s, and no terminal to ask for the superuser password", envBootstrapPassword)
		}
		p, err := promptNewPassword("Superuser password: ")
		if err != nil {
			return c, fmt.Errorf("reading password: %w", err)
		}
		c.password, source = p, "superuser password"
	}
	if err := kernel.ValidateCredentials(c.password, ""); err != nil {
		return c, fmt.Errorf("%s: %w", source, err)
	}
	held, herr := rail.LoadKey(kernelKeyPath())
	if herr != nil && !os.IsNotExist(herr) {
		return c, herr
	}
	phrase, source := os.Getenv(envBootstrapPhrase), envBootstrapPhrase
	if phrase == "" {
		if !interactiveTTY() {
			return c, fmt.Errorf("no %s, and no terminal to ask for the recovery phrase.\n"+
				"       A kernel is created from a 12-word recovery phrase, and it is never printed to a log:\n"+
				"       set the variable, or create the kernel at a terminal", envBootstrapPhrase)
		}
		ask := "Recovery phrase (Enter to create a new kernel): "
		if held != nil {
			ask = "This kernel was not finished being created. Its recovery phrase: "
		}
		p, err := promptMnemonic(ask)
		if err != nil {
			return c, fmt.Errorf("reading recovery phrase: %w", err)
		}
		phrase, source = p, "recovery phrase"
	}
	restored := phrase != ""
	if !restored {
		if held != nil {
			return c, fmt.Errorf("%s holds a kernel's key, so only that kernel's recovery phrase finishes creating it", kernelHome())
		}
		fresh, _, err := generateRecovery()
		if err != nil {
			return c, err
		}
		if err := showPhrase("Recovery phrase", fresh); err != nil {
			return c, err
		}
		phrase = fresh
	}
	key, err := deriveKernelKey(phrase)
	if err != nil {
		return c, fmt.Errorf("%s: %w", source, err)
	}
	recovery, err := deriveRecoveryKey(phrase)
	if err != nil {
		return c, fmt.Errorf("%s: %w", source, err)
	}
	if held != nil && !held.Equal(key) {
		return c, fmt.Errorf("%s: that is the phrase of kernel %s, and %s holds the key of kernel %s",
			source, kernel.PublicKey(key), kernelHome(), kernel.PublicKey(held))
	}
	c.key = key
	c.recoveryKey = base64.RawURLEncoding.EncodeToString(recovery.Public().(ed25519.PublicKey))
	if restored {
		fmt.Fprintf(os.Stderr, "That phrase is kernel %s", kernel.PublicKey(key))
		if w.Rail == rail.RailEVM {
			fmt.Fprintf(os.Stderr, ", paid at %s", strings.ToLower(ethcrypto.PubkeyToAddress(key.PublicKey).Hex()))
		}
		fmt.Fprintln(os.Stderr, ".")
	}
	return c, nil
}

// firstBoot creates the superuser of the kernel the credentials are for, and records whose
// database this is.
func firstBoot(ctx context.Context, k *kernel.Kernel, c bootCredentials) error {
	if err := k.FirstBoot(ctx, c.password, c.recoveryKey, c.key); err != nil {
		return fmt.Errorf("first boot: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Superuser %q created.\n", superuserHandle)
	return nil
}

// loadKernelKey reads the key of a kernel that exists. It is read once, by serve: the signer, the
// network identity and the rail all take what it returns.
func loadKernelKey() (*ecdsa.PrivateKey, error) {
	key, err := rail.LoadKey(kernelKeyPath())
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%s holds a kernel but not its key (%s).\n"+
			"       A kernel made by a juice before one key served as its identity and its payment address\n"+
			"       cannot be carried over: serve it with the juice that made it, or create a new kernel.\n"+
			"       If the file was lost, put it back from your backup", kernelHome(), rail.KeyFile)
	}
	return key, err
}

// bootstrap runs the idempotent startup tasks of a kernel whose superuser exists.
func bootstrap(k *kernel.Kernel, key *ecdsa.PrivateKey, nativeCfg NativeConfig, specs []native.Spec, net kernel.Network) error {
	ctx := context.Background()

	handle, err := k.GetConfig(ctx, configKeySuperuser)
	if err != nil || handle == "" {
		return fmt.Errorf("superuser_handle missing from config; the first boot did not complete")
	}

	// The key on disk and the database must be one kernel: signing as one over the other's ledger
	// would speak for books it never kept.
	if stored := k.SelfKey(ctx); key == nil || stored != kernel.PublicKey(key) {
		return fmt.Errorf("%s is the key of kernel %s, and this database is kernel %s",
			kernelKeyPath(), kernel.PublicKey(key), stored)
	}
	su, err := k.ReadUserByHandle(ctx, handle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}
	k.SetSigningKey(key, su.ID)

	// The kernel's own name is a petname on its own key (D15): what its users are addressed by,
	// what gossip and /health say, and what no peer can be given.
	if err := k.BindOwnName(ctx, globalCfg.KernelHandle); err != nil {
		return err
	}

	// Recover interrupted calls and re-park crashed task completions (after signing key is set).
	if err := k.Recover(ctx); err != nil {
		return fmt.Errorf("recover: %w", err)
	}

	// Retry any remote proxy calls that were pending at last shutdown.
	k.RetryPendingRemoteDispatches(ctx)

	// Every stored schema is read in one form, whatever release wrote it (D4).
	if err := k.CanonicalizeStoredSchemas(ctx); err != nil {
		return fmt.Errorf("canonicalize schemas: %w", err)
	}

	for _, spec := range specs {
		if err := ensureSysNative(ctx, k, handle, spec, nativeCfg.PriceOf(spec.Name)); err != nil {
			return err
		}
	}

	return nil
}

// ensureSysNative idempotently registers, activates, and grants local call access to a @sys native
// action. The contract comes from the native's own Spec (§9, one owner per native) and the price
// from configuration (§14), so drift in either is corrected on every boot.
func ensureSysNative(ctx context.Context, k *kernel.Kernel, superuserHandle string, spec native.Spec, price int64) error {
	su, err := k.ReadUserByHandle(ctx, superuserHandle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}
	a, err := k.ReadActionByOwnerName(ctx, su.ID, spec.Name)
	if err != nil || a == nil {
		a, err = k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
			OwnerUserID: su.ID,
			Name:        spec.Name,
			Title:       spec.Title,
			Kind:        kernel.KindNative,
			Price:       price,
			Effect:      spec.Effect,
		})
		if err != nil {
			return fmt.Errorf("create @sys/%s: %w", spec.Name, err)
		}
	}
	if err := k.ActivateNativeAction(ctx, a.ID, spec.Title, spec.Description, spec.InputSchema, spec.OutputSchema, price, spec.Effect); err != nil {
		return fmt.Errorf("activate @sys/%s: %w", spec.Name, err)
	}
	return nil
}
