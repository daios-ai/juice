// Package main is the CLI entrypoint for the Juice kernel.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	flagQuiet  bool
)

func init() {
	rootCmd.PersistentFlags().StringVar(&flagDB, "db", envOr("JUICE_DB_PATH", "juice.db"), "SQLite database path")
	rootCmd.PersistentFlags().StringVar(&flagOutput, "output", "text", "Output format: text or json")
	rootCmd.PersistentFlags().BoolVar(&flagQuiet, "quiet", false, "Print only the created resource ID")
}

func main() {
	if err := loadConfigFile(); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// loadConfigFile reads an optional JSON config file and sets any missing environment
// variables from it. File path comes from JUICE_CONFIG_FILE, defaulting to juice.json.
// Environment variables always take precedence over file values.
func loadConfigFile() error {
	path := os.Getenv("JUICE_CONFIG_FILE")
	if path == "" {
		path = "juice.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config file %q: %w", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}
	for k, v := range cfg {
		if os.Getenv(k) == "" {
			switch val := v.(type) {
			case string:
				_ = os.Setenv(k, val)
			default:
				_ = os.Setenv(k, fmt.Sprintf("%v", val))
			}
		}
	}
	return nil
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
	if secret := os.Getenv("JUICE_SECRET_KEY"); secret != "" {
		cfg.TokenSecret = secret
	} else if stored, _ := db.GetConfig(context.Background(), "jwt_secret"); stored != "" {
		cfg.TokenSecret = stored
	}
	// If neither is set, FirstBoot will generate and set the secret via SetTokenSecret.
	cfg.FeeRecipientID = os.Getenv("JUICE_FEE_RECIPIENT")
	if bps := os.Getenv("JUICE_FEE_BPS"); bps != "" {
		v, err := strconv.ParseInt(bps, 10, 64)
		if err != nil {
			db.Close()
			return nil, nil, fmt.Errorf("JUICE_FEE_BPS invalid: %w", err)
		}
		if v < 0 || v > 10000 {
			db.Close()
			return nil, nil, fmt.Errorf("JUICE_FEE_BPS must be 0-10000")
		}
		cfg.FeeBPS = v
	}
	if cfg.FeeBPS > 0 && cfg.FeeRecipientID == "" {
		db.Close()
		return nil, nil, fmt.Errorf("JUICE_FEE_RECIPIENT required when JUICE_FEE_BPS > 0")
	}
	if ttl := os.Getenv("JUICE_TOKEN_TTL"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil {
			db.Close()
			return nil, nil, fmt.Errorf("JUICE_TOKEN_TTL invalid: %w", err)
		}
		cfg.TokenTTL = d
	}
	if ms := os.Getenv("JUICE_SCRIPT_TIMEOUT_MS"); ms != "" {
		v, err := strconv.ParseInt(ms, 10, 64)
		if err != nil {
			db.Close()
			return nil, nil, fmt.Errorf("JUICE_SCRIPT_TIMEOUT_MS invalid: %w", err)
		}
		cfg.ScriptTimeout = time.Duration(v) * time.Millisecond
	}
	if mb := os.Getenv("JUICE_SCRIPT_MEMORY_BYTES"); mb != "" {
		v, err := strconv.ParseInt(mb, 10, 64)
		if err != nil {
			db.Close()
			return nil, nil, fmt.Errorf("JUICE_SCRIPT_MEMORY_BYTES invalid: %w", err)
		}
		cfg.ScriptMemory = v
	}
	if os.Getenv("JUICE_ALLOW_LOCAL_SOURCES") == "true" {
		cfg.AllowLocalSources = true
	}
	cfg.AuthIssuer = os.Getenv("JUICE_AUTH_ISSUER")
	cfg.AuthAudience = os.Getenv("JUICE_AUTH_AUDIENCE")

	logger, _ := log.New(log.Config{
		Level:    envOr("JUICE_LOG_LEVEL", "info"),
		FilePath: envOr("JUICE_LOG_FILE", ""),
		Format:   envOr("JUICE_LOG_FORMAT", "text"),
	})

	exec := script.New(script.Config{
		TimeoutMS:   cfg.ScriptTimeout.Milliseconds(),
		MemoryBytes: cfg.ScriptMemory,
	})

	ollamaURL := envOr("JUICE_OLLAMA_URL", "http://localhost:11434")
	embedder := kernel.Embedder(&llm.OllamaEmbedder{
		URL:   ollamaURL,
		Model: envOr("JUICE_OLLAMA_EMBED_MODEL", "nomic-embed-text"),
	})
	chatter := kernel.Chatter(&llm.OllamaChatter{
		URL:   ollamaURL,
		Model: envOr("JUICE_OLLAMA_CHAT_MODEL", "gemma4:26b"),
	})

	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, allowLocal: cfg.AllowLocalSources}
	k := kernel.New(db, exec, httpExec, embedder, chatter, cfg, logger)

	// Load signing key if present (best-effort; no error if not yet bootstrapped).
	if privB64, _ := db.GetConfig(context.Background(), configKeySigningPrivate); privB64 != "" {
		if privBytes, err := base64.RawURLEncoding.DecodeString(privB64); err == nil && len(privBytes) == ed25519.PrivateKeySize {
			suHandle, _ := db.GetConfig(context.Background(), configKeySuperuser)
			if su, err := db.ReadUserByHandle(context.Background(), suHandle); err == nil {
				k.SetSigningKey(ed25519.PrivateKey(privBytes), su.ID, suHandle)
			}
		}
	}

	// Wire signing callback into the HTTP executor; private key stays inside kernel.
	httpExec.signerFn = k.SignFederation

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

func saveFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func saveToken(tok string) error        { return saveFile(tokenPath(), tok) }
func removeToken() error                { return os.Remove(tokenPath()) }

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

func saveRefreshToken(tok string) error { return saveFile(refreshTokenPath(), tok) }
func removeRefreshToken() error         { return os.Remove(refreshTokenPath()) }

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

// requireSubjectID loads the stored access token and verifies it.
// If the access token is expired, it silently uses the refresh token to obtain
// a new one, saves both new tokens, and returns the subject ID.
// Returns ErrUnauthenticated if the account is suspended.
func requireSubjectID(k *kernel.Kernel) (string, error) {
	tok, err := loadToken()
	if err != nil {
		return "", err
	}
	subjectID, err := k.VerifyToken(tok)
	if err != nil {
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
		subjectID, err = k.VerifyToken(access)
		if err != nil {
			return "", err
		}
	}
	// Mirror authMiddleware: reject suspended accounts at every authenticated CLI call.
	u, uErr := k.ReadUser(context.Background(), subjectID)
	if uErr == nil && u.SuspendedAt != nil {
		return "", kernel.ErrUnauthenticated.Wrap("account suspended")
	}
	return subjectID, nil
}

// withKernel opens the kernel, calls fn, then closes the store.
func withKernel(fn func(*kernel.Kernel) error) error {
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(k)
}

// withSubject opens the kernel, resolves the authenticated subject, and calls fn.
func withSubject(fn func(*kernel.Kernel, string) error) error {
	return withKernel(func(k *kernel.Kernel) error {
		subjectID, err := requireSubjectID(k)
		if err != nil {
			return err
		}
		return fn(k, subjectID)
	})
}

// withSuperuser opens the kernel, requires the caller to be the superuser, and calls fn.
func withSuperuser(fn func(*kernel.Kernel, string) error) error {
	return withKernel(func(k *kernel.Kernel) error {
		operatorID, err := requireSuperuser(k)
		if err != nil {
			return err
		}
		return fn(k, operatorID)
	})
}

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

// printQuiet prints only the resource ID, used when --quiet is set.
func printQuiet(id string) {
	fmt.Println(id)
}

// readJSONArg parses a JSON argument string, supporting @file.json to read from a file.
func readJSONArg(s string) (map[string]any, error) {
	if s == "" || s == "{}" {
		return map[string]any{}, nil
	}
	if strings.HasPrefix(s, "@") {
		data, err := os.ReadFile(s[1:])
		if err != nil {
			return nil, fmt.Errorf("read file %s: %w", s[1:], err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("parse JSON from file: %w", err)
		}
		return m, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return m, nil
}

// jsonMarshalIndent is a helper for pretty-printing JSON.
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
