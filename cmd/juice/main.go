// Package main is the CLI entrypoint for the Juice kernel.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/native"
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
	flagConfig string
	flagOutput string
	flagQuiet  bool
)

// globalCfg is populated from the config file before any command runs.
var globalCfg ServerConfig

// resolvedConfigPath is the config file path resolved during initConfig.
var resolvedConfigPath string

func init() {
	rootCmd.PersistentFlags().StringVar(&flagDB, "db", "juice.db", "SQLite database path")
	rootCmd.PersistentFlags().StringVar(&flagConfig, "config", "", "JSON config file (default: juice.json in --db directory)")
	rootCmd.PersistentFlags().StringVar(&flagOutput, "output", "text", "Output format: text or json")
	rootCmd.PersistentFlags().BoolVar(&flagQuiet, "quiet", false, "Print only the created resource ID")
	cobra.OnInitialize(initConfig)
}

// initConfig loads the JSON config file and applies JUICE_* environment overrides.
// The path defaults to juice.json in the same directory as --db so that both
// files stay co-located. JUICE_DB_PATH overrides the --db flag default.
func initConfig() {
	if v := os.Getenv("JUICE_DB_PATH"); v != "" && flagDB == "juice.db" {
		flagDB = v
	}
	path := flagConfig
	if path == "" {
		path = filepath.Join(filepath.Dir(flagDB), "juice.json")
	}
	resolvedConfigPath = path
	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if err := applyEnvOverrides(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	globalCfg = cfg
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor maps KernelError codes to stable POSIX-friendly exit codes.
func exitCodeFor(err error) int {
	switch kernel.KernelErrorCode(err) {
	case "unauthenticated":
		return 2
	case "unauthorized":
		return 3
	case "not_found":
		return 4
	case "invalid_input", "schema_violation":
		return 5
	case "insufficient_funds":
		return 6
	case "timeout":
		return 7
	default:
		return 1
	}
}

// openKernel opens the SQLite store and constructs a Kernel from globalCfg.
// The caller is responsible for closing the store when done.
func openKernel() (*kernel.Kernel, *store.DB, *log.Logger, error) {
	db, err := store.Open(flagDB)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open db: %w", err)
	}

	cfg := kernel.DefaultConfig()

	// JWT secret: env var only (never stored in config file).
	if secret := os.Getenv("JUICE_SECRET_KEY"); secret != "" {
		cfg.TokenSecret = secret
	} else if stored, _ := db.GetConfig(context.Background(), "jwt_secret"); stored != "" {
		cfg.TokenSecret = stored
	}

	cfg.FeeBPS = globalCfg.FeeBPS
	if globalCfg.FeeBPS < 0 || globalCfg.FeeBPS > 10000 {
		db.Close()
		return nil, nil, nil, fmt.Errorf("fee_bps must be 0–10000")
	}

	cfg.ImportBPS = globalCfg.ImportBPS
	if globalCfg.ImportBPS < 0 || globalCfg.ImportBPS > 10000 {
		db.Close()
		return nil, nil, nil, fmt.Errorf("import_bps must be 0–10000")
	}

	tokenTTL, err := time.ParseDuration(globalCfg.TokenTTL)
	if err != nil {
		db.Close()
		return nil, nil, nil, fmt.Errorf("token_ttl invalid: %w", err)
	}
	cfg.TokenTTL = tokenTTL
	cfg.ScriptTimeout = time.Duration(globalCfg.ScriptTimeoutMS) * time.Millisecond
	cfg.ScriptMemory = globalCfg.ScriptMemoryBytes
	cfg.AllowLocalSources = globalCfg.AllowLocalSources
	cfg.AuthIssuer = globalCfg.AuthIssuer
	cfg.AuthAudience = globalCfg.AuthAudience

	logger, _ := log.New(log.Config{
		Level:    globalCfg.LogLevel,
		FilePath: globalCfg.LogFile,
		Format:   globalCfg.LogFormat,
	})

	exec := script.New(script.Config{
		TimeoutMS:   cfg.ScriptTimeout.Milliseconds(),
		MemoryBytes: cfg.ScriptMemory,
	})

	embedder := kernel.Embedder(&llm.OllamaEmbedder{
		URL:   globalCfg.Native.LLM.URL,
		Model: globalCfg.Native.LLM.EmbedModel,
	})
	chatter := kernel.Chatter(&llm.OllamaChatter{
		URL:   globalCfg.Native.LLM.URL,
		Model: globalCfg.Native.LLM.ChatModel,
	})

	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, allowLocal: cfg.AllowLocalSources}
	k := kernel.New(db, exec, httpExec, embedder, cfg, logger)

	// Wire credential encryption. Generate a key on first use (stored in config file).
	if globalCfg.CredentialsKey == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err == nil {
			globalCfg.CredentialsKey = base64.RawURLEncoding.EncodeToString(raw)
			_ = writeConfig(resolvedConfigPath, globalCfg) // best-effort persist
		}
	}
	if globalCfg.CredentialsKey != "" {
		if keyBytes, err := base64.RawURLEncoding.DecodeString(globalCfg.CredentialsKey); err == nil {
			if box, err := newAESGCMBox(keyBytes); err == nil {
				k.SetSecretBox(box)
				httpExec.secretBox = box
			}
		}
	}

	// Register native action plugins. Must happen on every kernel open, not just bootstrap.
	compiler := script.NewTinyGoCompiler(script.CompileConfig{})
	native.RegisterLookupHandler(k)
	native.RegisterChatHandler(k, chatter)
	native.RegisterMakeHandler(k, native.MakeDeps{
		Scripts:  exec,
		Compiler: compiler,
		Chatter:  chatter,
		Embedder: embedder,
	}, script.TinyGoSDK, globalCfg.Native.Make.MaxSteps)
	native.RegisterTimeHandler(k)
	native.RegisterSinkHandler(k)
	native.RegisterMessageHandler(k)
	native.RegisterRandomHandler(k)

	// Load signing key if present (best-effort; no error if not yet bootstrapped).
	if privB64, _ := db.GetConfig(context.Background(), configKeySigningPrivate); privB64 != "" {
		if privBytes, err := base64.RawURLEncoding.DecodeString(privB64); err == nil && len(privBytes) == ed25519.PrivateKeySize {
			if su, err := db.ReadUserByHandle(context.Background(), "@sys"); err == nil {
				k.SetSigningKey(ed25519.PrivateKey(privBytes), su.ID)
			}
		}
	}

	// Wire signing callback into the HTTP executor; private key stays inside kernel.
	httpExec.signerFn = k.SignFederation

	return k, db, logger, nil
}

// tokenDir returns a directory namespaced by the canonical DB path so that
// tokens from different kernels never collide, even in the same HOME.
func tokenDir() string {
	home, _ := os.UserHomeDir()
	abs, _ := filepath.Abs(flagDB)
	h := sha256.Sum256([]byte(abs))
	return filepath.Join(home, ".juice", "tokens", fmt.Sprintf("%x", h[:6]))
}

func tokenPath() string        { return filepath.Join(tokenDir(), "token") }
func refreshTokenPath() string { return filepath.Join(tokenDir(), "refresh_token") }

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

func saveToken(tok string) error { return saveFile(tokenPath(), tok) }
func removeToken() error         { return os.Remove(tokenPath()) }

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

// requireCallerID loads the stored access token and verifies it.
// If the access token is expired, it silently uses the refresh token to obtain
// a new one, saves both new tokens, and returns the caller ID.
// Returns ErrUnauthenticated if the account is suspended.
func requireCallerID(k *kernel.Kernel) (string, error) {
	tok, err := loadToken()
	if err != nil {
		return "", err
	}
	callerID, err := k.VerifyToken(tok)
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
		callerID, err = k.VerifyToken(access)
		if err != nil {
			return "", err
		}
	}
	// Mirror authMiddleware: reject suspended accounts at every authenticated CLI call.
	u, uErr := k.ReadUser(context.Background(), callerID)
	if uErr == nil && u.SuspendedAt != nil {
		return "", kernel.ErrUnauthenticated.Wrap("account suspended")
	}
	return callerID, nil
}

// withKernel opens the kernel, calls fn, then closes the store.
func withKernel(fn func(*kernel.Kernel) error) error {
	k, db, _, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(k)
}

// withCaller opens the kernel, resolves the authenticated caller, and calls fn.
func withCaller(fn func(*kernel.Kernel, string) error) error {
	return withKernel(func(k *kernel.Kernel) error {
		callerID, err := requireCallerID(k)
		if err != nil {
			return err
		}
		return fn(k, callerID)
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

