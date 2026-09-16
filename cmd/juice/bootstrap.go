// SPDX-License-Identifier: AGPL-3.0-only

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
	"github.com/daios-ai/juice/store"
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

// firstBootConfig gathers the configuration of a kernel that does not exist yet. It asks and
// nothing more: the caller writes, under the home's lock, which is the only write of config.json
// there is — every later boot reads it and leaves it alone, so a runtime-only override
// (JUICE_CREDENTIALS_KEY) never reaches the disk. What the operator pre-seeded stands; the rest is
// asked, because these are the facts a kernel cannot revise.
func firstBootConfig(name, home string) (ServerConfig, error) {
	path := filepath.Join(home, "config.json")
	cfg, err := LoadConfig(path)
	if err != nil && !os.IsNotExist(err) {
		return cfg, err
	}
	// A configuration file written in advance is the operator saying, in the only way a machine with
	// no terminal can, that this kernel should exist. Without one, they are asked — and told what is
	// already here, since a name that is not on that list is usually a name mistyped.
	if os.IsNotExist(err) {
		here := "No kernels here yet."
		if others := kernelsHere(); len(others) > 0 {
			here = "Kernels here: " + strings.Join(others, ", ") + "."
		}
		if !interactiveTTY() {
			return cfg, fmt.Errorf("there is no kernel named %s, and no terminal to ask. %s\n"+
				"       To create it without a terminal, write %s to %s and run this again",
				name, here, worldChoices(), path)
		}
		fmt.Fprintf(os.Stderr, "There is no kernel named %s. %s\n", name, here)
		if aerr := askYesNo(fmt.Sprintf("Create %s as a new kernel?", name)); aerr != nil {
			return cfg, aerr
		}
	}
	if cfg.KernelHandle == "" {
		cfg.KernelHandle = name
	}
	if cfg.World == "" {
		w, aerr := askWorld(name, path)
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
		rpc, aerr := ask(fmt.Sprintf("Where does this kernel reach the %s chain", world.Name), "rail_rpc", path)
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
	return cfg, nil
}

// worldChoices writes the `world` key the way it goes in the file, so a refusal can be obeyed by
// copying it rather than by translating a list of names into JSON.
func worldChoices() string {
	return `"world": "` + strings.Join(rail.Shipped, `", "`) + `"`
}

// askWorld asks which money a new kernel uses. The question is the operator's, not the code's: the
// choice is permanent, and each world says in its own words what it means, so nobody has to know
// that a network is called a world here or what is on the other end of the name.
func askWorld(name, path string) (string, error) {
	if !interactiveTTY() {
		return "", fmt.Errorf("kernel %s: no %q in %s and there is no terminal to ask.\n"+
			"       Write %s to that file and run this again", name, "world", path, worldChoices())
	}
	fmt.Fprintf(os.Stderr, "\nWhich money will %s use? This cannot be changed later.\n", name)
	for _, w := range rail.Shipped {
		world, err := rail.Load(w)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(os.Stderr, "  %-5s %s\n", w, world.Description)
	}
	for {
		choice, err := ask("Choice ["+strings.Join(rail.Shipped, "/")+"]", "world", path)
		if err != nil {
			return "", err
		}
		if _, lerr := rail.Load(choice); lerr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", lerr)
			continue
		}
		return choice, nil
	}
}

// ask reads one answer from the terminal, or refuses off one naming the key and the file that would
// have supplied it. A whole line is read, so an answer with a space reaches the validator that
// refuses it rather than being silently truncated.
func ask(prompt, key, path string) (string, error) {
	if !interactiveTTY() {
		return "", fmt.Errorf("no %q in %s, and no terminal to ask", key, path)
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

// worldFor decides which network this kernel serves. One kernel, one network, for life (D23):
// balances, receipts and debts mean one thing only, so the answer is fixed the first time and read
// from the database ever after. `configured` is config.json's `world`, needed only where the digest
// alone cannot name the network — a first boot, or a world file this build does not ship — and
// checked against the record wherever both exist.
//
// A kernel that already has a network is therefore never asked for one, which is what lets a
// database made before `world` was written into config.json go on serving.
func worldFor(ctx context.Context, db *store.DB, configured, configPath string) (rail.World, error) {
	stored, _ := db.GetConfig(ctx, configKeyWorldDigest)
	if made, _ := db.GetConfig(ctx, configKeySuperuser); stored == "" && made != "" {
		// A kernel with a superuser but no digest was made before networks existed, and belongs to
		// play, whose credits were always the operator's own records. Reading it as play's record
		// rather than as a special case is what keeps binding it to a token world impossible: credits
		// by fiat would become claims on a token.
		play, err := rail.Load("play")
		if err != nil {
			return rail.World{}, err
		}
		stored = play.Network().Digest
	}
	if stored == "" { // a first boot: only the configuration can say
		if configured == "" {
			return rail.World{}, fmt.Errorf("no %q in %s", "world", configPath)
		}
		return rail.Load(configured)
	}
	was, known := shippedWorld(stored)
	if configured == "" {
		if !known {
			return rail.World{}, fmt.Errorf(
				"this kernel was created on a network this build does not ship; name its world file in %q of %s",
				"world", configPath)
		}
		return was, nil
	}
	// Named as well as recorded: the two must be the same network. They are compared by digest, so
	// the same world under a file path is the same network, and a different one is refused by name.
	w, err := rail.Load(configured)
	if err != nil {
		return rail.World{}, err
	}
	if w.Network().Digest != stored {
		name := "a world this build does not ship"
		if known {
			name = was.Name
		}
		return rail.World{}, fmt.Errorf("this kernel was created on network %s; config.json selects %s — "+
			"one kernel serves one network for life, so serve it as %s or create a new kernel", name, w.Name, name)
	}
	return w, nil
}

// shippedWorld is the world whose digest this is, among those this build carries. A digest is a
// hash, so naming the network it stands for is a lookup rather than a decoding.
func shippedWorld(digest string) (rail.World, bool) {
	for _, name := range rail.Shipped {
		if w, err := rail.Load(name); err == nil && w.Network().Digest == digest {
			return w, true
		}
	}
	return rail.World{}, false
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
