package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/native"
	"github.com/daios-ai/juice/rail"
	"golang.org/x/term"
)

const (
	// configKeyWorldDigest records the network this database belongs to, written once and checked
	// at every startup (D9, D23).
	configKeyWorldDigest    = "world_digest"
	configKeySuperuser      = "superuser_handle"
	configKeySigningPublic  = "signing_public_key"
	configKeySigningPrivate = "signing_private_key"
	superuserHandle         = "sys"
)

// firstBootConfig produces the configuration of a kernel that does not exist yet, and is the only
// writer of config.json — every later boot reads it and leaves it alone, which is what keeps a
// runtime-only override (JUICE_CREDENTIALS_KEY) from ever reaching the disk. What the operator has
// pre-seeded stands; the rest is asked, because these are the facts a kernel cannot revise.
func firstBootConfig(name, home string) (ServerConfig, error) {
	cfg, err := LoadConfig(filepath.Join(home, "config.json"))
	if err != nil && !os.IsNotExist(err) {
		return cfg, err
	}
	fmt.Fprintf(os.Stderr, "First boot of kernel %s at %s.\n", name, home)
	fmt.Fprintf(os.Stderr, "A new signing key is minted here; its nickname, its network and that key are fixed for the life of the kernel.\n")

	if cfg.KernelHandle == "" {
		cfg.KernelHandle = name
	}
	if cfg.World == "" {
		w, aerr := ask("World — play, test, real, or a world file", "world")
		if aerr != nil {
			return cfg, aerr
		}
		cfg.World = w
	}
	world, err := rail.Load(cfg.World)
	if err != nil {
		return cfg, err
	}
	if world.Chained() && cfg.RailRPC == "" {
		rpc, aerr := ask(fmt.Sprintf("Chain endpoint for %s (rail_rpc)", world.Name), "rail_rpc")
		if aerr != nil {
			return cfg, aerr
		}
		cfg.RailRPC = rpc
	}
	if cfg.CredentialsKey == "" {
		raw := make([]byte, 32)
		if _, rerr := rand.Read(raw); rerr != nil {
			return cfg, fmt.Errorf("generate credentials key: %w", rerr)
		}
		cfg.CredentialsKey = base64.RawURLEncoding.EncodeToString(raw)
	}
	// The home itself is holdHome's to make, and it has: this runs under that lock.
	return cfg, writeConfig(filepath.Join(home, "config.json"), cfg)
}

// ask reads one answer from the terminal, or refuses off one naming the configuration key that
// would have supplied it. A whole line is read, so an answer with a space reaches the validator
// that refuses it rather than being silently truncated.
func ask(prompt, key string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("no %q in config.json, and no terminal to ask on", key)
	}
	for {
		fmt.Fprintf(os.Stderr, "%s: ", prompt)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if v := strings.TrimSpace(line); v != "" {
			return v, nil
		}
		if err != nil {
			return "", fmt.Errorf("%s is required", key)
		}
	}
}

// bindWorld records the network this database belongs to, or refuses one it does not: one kernel,
// one network, for life (D23), since its balances, receipts and debts mean one thing only. It runs
// after the rail has verified the world, so the digest names a network that was checked rather than
// one that was merely configured. A database that predates the rail belongs to play, whose credits
// were always the operator's own records; binding it to a token world would turn them into claims
// on real money.
func bindWorld(ctx context.Context, k *kernel.Kernel, net kernel.Network) error {
	stored, _ := k.GetConfig(ctx, configKeyWorldDigest)
	if stored == net.Digest {
		return nil
	}
	if stored == "" {
		play, err := rail.Load("play")
		if err != nil {
			return err
		}
		// A kernel with a superuser but no digest was made before networks existed, so it is play's.
		if made, _ := k.GetConfig(ctx, configKeySuperuser); made != "" && net.Digest != play.Network().Digest {
			return fmt.Errorf("this database was made before networks existed, so it belongs to play; "+
				"config selects %q — start a new kernel for that network instead", net.Name)
		}
		return k.SetConfig(ctx, configKeyWorldDigest, net.Digest)
	}
	return fmt.Errorf("this kernel was created on network %s; its configuration now selects %s — "+
		"one kernel serves one network for life, so serve it as %s or create a new kernel",
		worldNameOf(stored), net.Name, worldNameOf(stored))
}

// worldNameOf names a recorded digest, so a refusal says which network the database belongs to
// rather than only which one the configuration asked for.
func worldNameOf(digest string) string {
	for _, name := range []string{"play", "test", "real"} {
		if w, err := rail.Load(name); err == nil && w.Network().Digest == digest {
			return name
		}
	}
	return "a world this build does not ship"
}

// bootstrap runs the idempotent startup tasks, and on a first boot (no superuser configured) asks
// for the credentials that create one.
func bootstrap(k *kernel.Kernel, nativeCfg NativeConfig, specs []native.Spec, net kernel.Network) error {
	ctx := context.Background()

	handle, err := k.GetConfig(ctx, configKeySuperuser)
	if err != nil || handle == "" {
		if handle, err = firstBoot(ctx, k); err != nil {
			return err
		}
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
	pubKeyB64, _ := k.GetConfig(ctx, configKeySigningPublic)
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

	// Persist the kernel handle so GetGossip serves it from the DB.
	_ = k.SetConfig(ctx, "kernel_handle", globalCfg.KernelHandle)

	// Recover interrupted calls and re-park crashed step completions (after signing key is set).
	if err := k.Recover(ctx); err != nil {
		return fmt.Errorf("recover: %w", err)
	}

	// Retry any remote proxy calls that were pending at last shutdown.
	k.RetryPendingRemoteDispatches(ctx)

	for _, spec := range specs {
		if err := ensureSysNative(ctx, k, handle, spec, nativeCfg.PriceOf(spec.Name)); err != nil {
			return err
		}
	}

	return nil
}

func firstBoot(ctx context.Context, k *kernel.Kernel) (string, error) {
	password := os.Getenv("JUICE_BOOTSTRAP_PASSWORD")
	if password == "" {
		p, err := promptNewPassword("Superuser password: ")
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		password = p
	}
	if password == "" {
		return "", fmt.Errorf("password cannot be empty")
	}

	// Enroll sys's own recovery phrase (§12): generated client-side, only the public key is
	// stored, so the operator can reset the superuser password if it is lost. The ceremony shows
	// and acknowledges the phrase before committing, so no logging follows until the operator
	// has it (interactive), and a boot that fails after display announces the phrase is dead.
	if err := enrollRecovery("sys recovery phrase", func(recoveryPub string) error {
		return k.FirstBoot(ctx, password, recoveryPub)
	}); err != nil {
		return "", fmt.Errorf("first boot: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Superuser %q created.\n", superuserHandle)
	return superuserHandle, nil
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
			Kind:        kernel.KindNative,
			Price:       price,
			Effect:      spec.Effect,
		})
		if err != nil {
			return fmt.Errorf("create @sys/%s: %w", spec.Name, err)
		}
	}
	if err := k.ActivateNativeAction(ctx, a.ID, spec.Description, spec.InputSchema, spec.OutputSchema, price, spec.Effect); err != nil {
		return fmt.Errorf("activate @sys/%s: %w", spec.Name, err)
	}
	return nil
}
