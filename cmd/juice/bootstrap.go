package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/native"
	"golang.org/x/term"
)

const (
	configKeySuperuser      = "superuser_handle"
	configKeySigningPublic  = "signing_public_key"
	configKeySigningPrivate = "signing_private_key"
	superuserHandle         = "sys"
)

// bootstrap runs idempotent startup tasks before the server accepts requests.
// requireBareHandle rejects a kernel handle carrying a sigil or path separator; handles are bare
// (§3, §14), and this one is concatenated into gossip and references, so it is validated — not
// silently rewritten — wherever it enters (prompt, env, or config.json).
func requireBareHandle(h string) error {
	if strings.ContainsAny(h, "@/") {
		return fmt.Errorf("kernel handle %q must be bare (no @ or /)", h)
	}
	return nil
}

// requireKernelName resolves the handle this kernel presents to the network (§12, §13): the
// configured value, else JUICE_BOOTSTRAP_KERNEL_HANDLE, else a prompt that repeats until a name is
// given. There is no derived fallback — an unnamed kernel is a configuration error, not a default.
func requireKernelName() (string, error) {
	name := globalCfg.KernelHandle
	if name == "" {
		name = strings.TrimSpace(os.Getenv("JUICE_BOOTSTRAP_KERNEL_HANDLE"))
	}
	for name == "" && term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Kernel name — the handle this kernel presents to the network (required): ")
		var line string
		fmt.Fscanln(os.Stdin, &line)
		name = strings.TrimSpace(line)
	}
	if name == "" {
		return "", fmt.Errorf("kernel name is required: run interactively or set JUICE_BOOTSTRAP_KERNEL_HANDLE")
	}
	return name, requireBareHandle(name)
}

// On first boot (no superuser configured), it prompts for credentials interactively.
func bootstrap(k *kernel.Kernel, nativeCfg NativeConfig, specs []native.Spec) error {
	ctx := context.Background()

	// The name is resolved BEFORE anything is written, so a boot that cannot be named leaves no
	// half-created kernel behind: the next boot would find a superuser configured, skip first boot
	// entirely, and have nowhere left to demand a name.
	kernelName, err := requireKernelName()
	if err != nil {
		return err
	}
	if kernelName != globalCfg.KernelHandle {
		globalCfg.KernelHandle = kernelName
		_ = writeConfig(resolvedConfigPath, globalCfg)
	}

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
	// Announce the location loudly: a first boot mints a NEW kernel identity and signing
	// key, so an operator who launched against the wrong DB path (a fresh, unintended
	// federation identity) sees it here — including in headless mode, before any prompt.
	loc := flagDB
	if abs, err := filepath.Abs(flagDB); err == nil {
		loc = abs
	}
	fmt.Fprintf(os.Stderr, "First boot: creating a NEW kernel — new identity and signing key — at %s\n", loc)

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

	// Enroll sys's own recovery phrase (§12): generated here, only the public key is stored, so the
	// operator can reset the superuser password if it is lost. The phrase is shown once.
	mnemonic, recoveryPub, err := generateRecovery()
	if err != nil {
		return "", fmt.Errorf("generate recovery phrase: %w", err)
	}
	if err := k.FirstBoot(ctx, password, recoveryPub); err != nil {
		return "", fmt.Errorf("first boot: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Superuser %q created.\n", superuserHandle)
	fmt.Fprintln(os.Stderr, "sys recovery phrase (write this down; it is shown only once and cannot be recovered):")
	fmt.Fprintf(os.Stderr, "  %s\n", mnemonic)
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
