package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/log"
	"github.com/spf13/cobra"
)

// `juice seed` runs a bootstrap + relay helper node (§13): a DHT server that lets kernels find
// each other by key, plus a circuit-relay service for peers behind strict NAT. It runs no
// kernel and stores no Juice data. The same binary serves the flow harness on loopback and a
// public helper node in production. It prints its own multiaddr(s) — a `bootstrap_peers` entry
// for kernels — and logs `seed.ready` so the flow harness can scrape the address.
func init() {
	var addr, keyB64 string
	var allowLocal bool
	seedCmd := &cobra.Command{
		Use:   "seed",
		Short: "Run a federation bootstrap + relay helper node",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSeed(addr, keyB64, allowLocal)
		},
	}
	seedCmd.Flags().StringVar(&addr, "addr", "/ip4/0.0.0.0/tcp/0", "libp2p listen multiaddr")
	seedCmd.Flags().StringVar(&keyB64, "key", "", "base64url ed25519 private key for a stable peer ID (default: ephemeral)")
	seedCmd.Flags().BoolVar(&allowLocal, "allow-local", false, "permit private/loopback addresses (dev/flows)")
	rootCmd.AddCommand(seedCmd)
}

func runSeed(addr, keyB64 string, allowLocal bool) error {
	var key ed25519.PrivateKey
	if keyB64 != "" {
		raw, err := base64.RawURLEncoding.DecodeString(keyB64)
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return fmt.Errorf("--key must be a base64url ed25519 private key")
		}
		key = ed25519.PrivateKey(raw)
	} else {
		_, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		key = k
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seed, err := fed.NewSeed(ctx, key, []string{addr}, allowLocal)
	if err != nil {
		return fmt.Errorf("start seed: %w", err)
	}
	defer seed.Close()

	// seed.ready carries the multiaddrs — the harness scrapes this line, and an operator copies
	// one into a kernel's bootstrap_peers.
	addrs := seed.Addrs()
	logger, _ := log.New(log.Config{Level: globalCfg.LogLevel, FilePath: globalCfg.LogFile, Format: globalCfg.LogFormat})
	logger.Info("seed.ready", "addrs", addrs)
	for _, a := range addrs {
		fmt.Println(a)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()
	logger.Info("seed.shutdown")
	return nil
}
