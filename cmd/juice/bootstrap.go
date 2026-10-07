// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	configKeySigningPrivate   = "signing_private_key"
	superuserHandle           = "sys"
)

// firstBootConfig gathers the configuration of a kernel that does not exist yet. It asks and
// nothing more: the caller writes, under the home's lock, which is the only write of config.json a
// boot makes — every later boot reads it and leaves it alone, and `admin kernel config` writes the
// file and never an override, so a runtime-only one (JUICE_CREDENTIALS_KEY) never reaches the
// disk. What the operator pre-seeded stands; the rest is asked, because these are the facts a
// kernel cannot revise.
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
	if verr := kernel.ValidateHandle(cfg.KernelHandle); verr != nil {
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
		if verr := kernel.ValidateHandle(h); verr != nil {
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

// bootCredentials are the superuser's: its password, and the public half of a recovery phrase its
// client holds — empty when the kernel, being its own client, is to generate the phrase (D9).
type bootCredentials struct{ password, recoveryKey string }

const (
	envBootstrapPassword    = "JUICE_BOOTSTRAP_PASSWORD"
	envBootstrapRecoveryKey = "JUICE_BOOTSTRAP_RECOVERY_KEY"
)

// firstBootCredentials gathers the superuser's credentials and checks them by the rule every
// account is created under, writing nothing, so a boot refused here leaves nothing behind. Each
// has one source off a terminal, its variable, and a refusal names that variable; at a terminal
// the password is asked. A recovery key is never asked: without one the phrase is generated at
// creation, where it can be shown.
func firstBootCredentials() (bootCredentials, error) {
	c := bootCredentials{password: os.Getenv(envBootstrapPassword), recoveryKey: os.Getenv(envBootstrapRecoveryKey)}
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
	if err := kernel.ValidateCredentials(c.password, c.recoveryKey); err != nil {
		return c, fmt.Errorf("%s: %w", envBootstrapRecoveryKey, err)
	}
	return c, nil
}

// firstBoot creates the superuser. A recovery key it was given is enrolled as it stands and no
// phrase exists here; without one the kernel is the client and runs the ceremony a client runs —
// generate the phrase, show it, enroll the public half — on the terminal when there is one.
func firstBoot(ctx context.Context, k *kernel.Kernel, c bootCredentials) error {
	commit := func(recoveryPub string) error { return k.FirstBoot(ctx, c.password, recoveryPub) }
	var err error
	if c.recoveryKey != "" {
		err = commit(c.recoveryKey)
	} else {
		err = enrollRecovery("sys recovery phrase", commit)
	}
	if err != nil {
		return fmt.Errorf("first boot: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Superuser %q created.\n", superuserHandle)
	return nil
}

// bootstrap runs the idempotent startup tasks of a kernel whose superuser exists.
func bootstrap(k *kernel.Kernel, nativeCfg NativeConfig, specs []native.Spec, net kernel.Network) error {
	ctx := context.Background()

	handle, err := k.GetConfig(ctx, configKeySuperuser)
	if err != nil || handle == "" {
		return fmt.Errorf("superuser_handle missing from config; the first boot did not complete")
	}

	// Verify both signing keys are present, valid, and consistent.
	privKeyB64, _ := k.GetConfig(ctx, configKeySigningPrivate)
	if privKeyB64 == "" {
		return fmt.Errorf("signing_private_key missing from config; re-run on a fresh database or restore the key")
	}
	privKeyBytes, err := base64.RawURLEncoding.DecodeString(privKeyB64)
	if err != nil || len(privKeyBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing_private_key in config is invalid")
	}
	pubKeyB64 := k.SelfKey(ctx)
	if pubKeyB64 == "" {
		return fmt.Errorf("signing_public_key missing from config")
	}
	pubKeyBytes, err := base64.RawURLEncoding.DecodeString(pubKeyB64)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("signing_public_key in config is invalid")
	}
	derivedPub := ed25519.PrivateKey(privKeyBytes).Public().(ed25519.PublicKey)
	if !derivedPub.Equal(ed25519.PublicKey(pubKeyBytes)) {
		return fmt.Errorf("signing_public_key does not match signing_private_key")
	}

	// Load the signing key and issuer user ID into the kernel.
	su, err := k.ReadUserByHandle(ctx, handle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}
	k.SetSigningKey(ed25519.PrivateKey(privKeyBytes), su.ID)

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
