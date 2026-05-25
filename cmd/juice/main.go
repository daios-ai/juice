// Package main is the CLI entrypoint for the Juice kernel.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/script"
	"github.com/daios-ai/juice/store"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	version = "dev"
	commit  = "unknown"
)

var rootCmd = &cobra.Command{
	Use:     "juice",
	Short:   "Juice kernel — callable action platform",
	Version: version + " (" + commit + ")",
}

// Global flags.
var (
	flagDB     string
	flagOutput string
)

func init() {
	rootCmd.PersistentFlags().StringVar(&flagDB, "db", envOr("JUICE_DB_PATH", "juice.db"), "SQLite database path")
	rootCmd.PersistentFlags().StringVar(&flagOutput, "output", "text", "Output format: text or json")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// openKernel opens the SQLite store and constructs a Kernel with defaults.
// The caller is responsible for closing the store when done.
func openKernel() (*kernel.Kernel, *store.DB, error) {
	db, err := store.Open(flagDB)
	if err != nil {
		return nil, nil, fmt.Errorf("open db: %w", err)
	}

	cfg := kernel.DefaultConfig()
	cfg.TokenSecret = envOr("JUICE_SECRET_KEY", "dev-secret-change-me")
	cfg.FeeRecipientID = os.Getenv("JUICE_FEE_RECIPIENT")
	if bps := os.Getenv("JUICE_FEE_BPS"); bps != "" {
		if v, err := strconv.ParseInt(bps, 10, 64); err == nil {
			cfg.FeeBPS = v
		}
	}

	logger, _ := log.New(log.Config{
		Level:    envOr("JUICE_LOG_LEVEL", "info"),
		FilePath: envOr("JUICE_LOG_FILE", ""),
		Format:   "text",
	})

	exec := script.New(script.Config{
		TimeoutMS:   cfg.ScriptTimeout.Milliseconds(),
		MemoryBytes: cfg.ScriptMemory,
	})

	var embedder kernel.Embedder
	if ollamaURL := os.Getenv("JUICE_OLLAMA_URL"); ollamaURL != "" {
		embedder = &llm.OllamaEmbedder{
			URL:   ollamaURL,
			Model: envOr("JUICE_OLLAMA_EMBED_MODEL", "nomic-embed-text"),
		}
	}

	k := kernel.New(db, exec, embedder, cfg, logger)
	return k, db, nil
}

func tokenPath() string {
	home, _ := os.UserHomeDir()
	return home + "/.juice/token"
}

func loadToken() (string, error) {
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return "", fmt.Errorf("not logged in; run: juice auth login")
	}
	return string(data), nil
}

func saveToken(tok string) error {
	dir := tokenPath()[:len(tokenPath())-len("/token")]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(tokenPath(), []byte(tok), 0o600)
}

func removeToken() error {
	return os.Remove(tokenPath())
}

func refreshTokenPath() string {
	home, _ := os.UserHomeDir()
	return home + "/.juice/refresh_token"
}

func loadRefreshToken() (string, error) {
	data, err := os.ReadFile(refreshTokenPath())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func saveRefreshToken(tok string) error {
	dir := tokenPath()[:len(tokenPath())-len("/token")]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(refreshTokenPath(), []byte(tok), 0o600)
}

func removeRefreshToken() error {
	return os.Remove(refreshTokenPath())
}

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

// requireSubjectID loads the stored access token and verifies it.
// If the access token is expired, it silently uses the refresh token to obtain
// a new one, saves both new tokens, and returns the subject ID.
func requireSubjectID(k *kernel.Kernel) (string, error) {
	tok, err := loadToken()
	if err != nil {
		return "", err
	}
	subjectID, err := k.VerifyToken(tok)
	if err == nil {
		return subjectID, nil
	}
	// Access token invalid — attempt silent refresh.
	rt, rtErr := loadRefreshToken()
	if rtErr != nil {
		return "", fmt.Errorf("session expired; run: juice auth login")
	}
	access, newRT, rtErr := k.RefreshAccessToken(context.Background(), rt)
	if rtErr != nil {
		return "", fmt.Errorf("session expired; run: juice auth login")
	}
	if err := saveToken(access); err != nil {
		return "", err
	}
	_ = saveRefreshToken(newRT)
	return k.VerifyToken(access)
}

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}
