// Package main is the CLI entrypoint for the Juice kernel.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	// main() is the single place errors and usage are printed. Cobra prints neither itself,
	// so every command is handled identically: main renders the error, and shows usage only
	// for flag/argument mistakes (see enteredCommand and main).
	SilenceUsage:  true,
	SilenceErrors: true,
	// PersistentPreRun fires after flag parsing and argument validation pass, just before a
	// command body runs. Marking that boundary lets main distinguish a syntax error (usage
	// worth showing) from a runtime error (usage would be noise). No subcommand overrides
	// this, so the behavior is uniform across every command.
	PersistentPreRun: func(_ *cobra.Command, _ []string) { enteredCommand = true },
	// Hide cobra's stock `completion` command from the help listing (it still works if invoked).
	CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
}

// enteredCommand becomes true once a matched command clears flag/argument validation and its
// body starts. If Execute returns an error while it is still false, the failure came from
// parsing — so main prints usage to show the correct syntax.
var enteredCommand bool

// Global flags.
var (
	flagDB      string
	flagConfig  string
	flagJSON    bool
	flagQuiet   bool
	flagServer  string
	flagVerbose bool
)

// globalCfg is populated from the config file before any command runs.
var globalCfg ServerConfig

// resolvedConfigPath is the config file path resolved during initConfig.
var resolvedConfigPath string

func init() {
	rootCmd.PersistentFlags().StringVar(&flagDB, "db", "", "SQLite database path (default $JUICE_HOME/kernel/juice.db)")
	rootCmd.PersistentFlags().StringVar(&flagConfig, "config", "", "Config file path")
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "Output JSON instead of human-readable text")
	rootCmd.PersistentFlags().BoolVar(&flagQuiet, "quiet", false, "Print only the created resource ID")
	rootCmd.PersistentFlags().StringVar(&flagServer, "server", "", "Server base URL")
	rootCmd.PersistentFlags().BoolVar(&flagVerbose, "verbose", false, "Show underlying error causes")
	cobra.OnInitialize(initConfig)
}

// juiceHome is the single root under which every juice-family binary keeps its state:
// $JUICE_HOME if set, else ~/.juice. It is fixed and absolute — never cwd-relative — so
// the kernel attaches to the same identity and signing key wherever it is launched, like
// Geth's ~/.ethereum or IPFS's ~/.ipfs. The fallback used when the home directory cannot
// be determined stays absolute (system temp) rather than the working directory, preserving
// that invariant in minimal environments.
func juiceHome() string {
	if h := os.Getenv("JUICE_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "juice")
	}
	return filepath.Join(home, ".juice")
}

// kernelHome is this component's subdirectory under the shared root, $JUICE_HOME/kernel.
// One subdirectory per component (kernel/, ui/, …) lets the whole suite back up and
// relocate as a unit while each binary owns its own namespace.
func kernelHome() string { return filepath.Join(juiceHome(), "kernel") }

// defaultDBPath is the per-user default database location, $JUICE_HOME/kernel/juice.db.
// Its config (config.json) and tokens share this directory.
func defaultDBPath() string { return filepath.Join(kernelHome(), "juice.db") }

// cacheDir is the reserved purgeable subdirectory for regenerable data (indexes, compiled
// artifacts, scratch). It is safe to delete; writers MkdirAll it on demand.
func cacheDir() string { return filepath.Join(kernelHome(), "cache") }

// initConfig loads the JSON config file. The DB path is the --db flag, else the per-user
// default $JUICE_HOME/kernel/juice.db; a fixed default means `juice serve` attaches to the
// same kernel — and the same signing key / federation identity — regardless of the working
// directory, instead of silently booting a fresh identity from whatever folder it happens
// to run in. The config path is --config, else config.json co-located with the DB.
func initConfig() {
	if flagDB == "" {
		flagDB = defaultDBPath()
	}
	path := flagConfig
	if path == "" {
		path = filepath.Join(filepath.Dir(flagDB), "config.json")
	}
	resolvedConfigPath = path
	// The juice home holds the config (credentials key) and, alongside it, the DB
	// (signing key) and tokens — create it 0700 so the default config can be written.
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		renderError(kernel.ErrInvalidInput.Wrapf("config: %v", err))
		os.Exit(exitCodeFor(kernel.ErrInvalidInput))
	}
	applyEnvOverrides(&cfg)
	globalCfg = cfg
}

func main() {
	// ExecuteC returns the command that actually ran/failed, so usage (when shown) is that
	// command's, not the root's.
	cmd, err := rootCmd.ExecuteC()
	if err != nil {
		renderError(err)
		if !enteredCommand {
			// The error came from flag parsing or argument validation, before the command
			// body ran: show how to invoke it correctly. Usage goes to stderr so stdout stays
			// payload-only (§14).
			fmt.Fprintln(os.Stderr)
			fmt.Fprint(os.Stderr, cmd.UsageString())
		}
		os.Exit(exitCodeFor(err))
	}
}

// ANSI colors for terminal error output; applied only when useColor reports a color-capable
// stderr, so piped or redirected output stays plain.
const (
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

// useColor reports whether error output should be colorized: only when NO_COLOR is unset and
// stderr is an interactive terminal (never for pipes, files, or CI).
func useColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// errorLine formats the one-line error message, in red when color is enabled.
func errorLine(msg string, color bool) string {
	line := "error: " + msg
	if color {
		return ansiRed + line + ansiReset
	}
	return line
}

// renderError is the single place CLI errors are printed: "error: <message>" to stderr, once,
// in red on a terminal. Usage (when applicable) is printed separately by main. With --verbose
// it also prints the underlying cause chain, so the friendly message stays clean by default
// while raw detail (e.g. a dial error) remains available for troubleshooting.
func renderError(err error) {
	fmt.Fprintln(os.Stderr, errorLine(err.Error(), useColor()))
	if flagVerbose {
		for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(cause) {
			fmt.Fprintln(os.Stderr, "  caused by:", cause.Error())
		}
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
	case "grant_required":
		return 8
	case "peer_unreachable":
		return 9
	case "peer_unfunded":
		return 10
	default:
		return 1
	}
}

// openKernel opens the SQLite store and constructs a Kernel from globalCfg.
// The caller is responsible for closing the store when done.
func openKernel() (*kernel.Kernel, *store.DB, *log.Logger, *httpActionExecutor, error) {
	if dir := filepath.Dir(flagDB); dir != "" {
		_ = os.MkdirAll(dir, 0o700)
	}
	// Reserve the purgeable cache subdir so the component layout exists for any writer.
	_ = os.MkdirAll(cacheDir(), 0o700)
	db, err := store.Open(flagDB)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("open db: %w", err)
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
		return nil, nil, nil, nil, fmt.Errorf("fee_bps must be 0–10000")
	}

	cfg.ImportBPS = globalCfg.ImportBPS
	if globalCfg.ImportBPS < 0 || globalCfg.ImportBPS > 10000 {
		db.Close()
		return nil, nil, nil, nil, fmt.Errorf("import_bps must be 0–10000")
	}

	tokenTTL, err := time.ParseDuration(globalCfg.TokenTTL)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, fmt.Errorf("token_ttl invalid: %w", err)
	}
	cfg.TokenTTL = tokenTTL
	cfg.ScriptTimeout = time.Duration(globalCfg.ScriptTimeoutMS) * time.Millisecond
	cfg.ScriptMemory = globalCfg.ScriptMemoryBytes
	cfg.AllowLocalSources = globalCfg.AllowLocalSources
	cfg.AuthIssuer = globalCfg.AuthIssuer
	cfg.AuthAudience = globalCfg.AuthAudience
	cfg.PeerRetention = globalCfg.peerRetention()

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
	ollamaChatter := &llm.OllamaChatter{
		URL:   globalCfg.Native.LLM.URL,
		Model: globalCfg.Native.LLM.ChatModel,
	}
	chatter := kernel.Chatter(ollamaChatter)

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
				// The §9 authenticator shares the box (to open sealed auth configs and grant tokens)
				// and reads/rotates grants through the store (§8).
				httpExec.auth = newAuthenticator(box, db, cfg.AllowLocalSources, cfg.ScriptTimeout)
				httpExec.auth.refFn = k.ActionRef // qualified @owner/name in grant-required errors
			}
		}
	}

	// Register native action plugins. Must happen on every kernel open, not just bootstrap.
	compiler := script.NewTinyGoCompiler(script.CompileConfig{})
	native.RegisterLookupHandler(k)
	native.RegisterChatHandler(k, chatter)
	native.RegisterEmbedHandler(k, embedder)
	native.RegisterJSONHandler(k, ollamaChatter)
	native.RegisterDecideHandler(k, ollamaChatter)
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
	webUA := globalCfg.Native.Web.UserAgent
	native.RegisterWebHandler(k, native.WebDeps{
		Fetch: func(ctx context.Context, url string) (int, []byte, string, string, error) {
			return httpExec.fetchWeb(ctx, url, webUA)
		},
	})
	native.RegisterTinyGoCompileHandler(k, native.CompileDeps{Compiler: compiler, Scripts: exec}, script.TinyGoSDK)

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

	return k, db, logger, httpExec, nil
}

// tokenDir returns a directory namespaced by the canonical DB path so that tokens for
// different kernels never collide, even in the same HOME. A token authenticates a user
// against a specific kernel (verified by that kernel's JWT secret), so keying by the DB
// path keeps it stable regardless of which server address the client talks to, and lets
// the local admin path and the HTTP client share one token for the same --db.
func tokenDir() string {
	abs, _ := filepath.Abs(flagDB)
	h := sha256.Sum256([]byte(abs))
	return filepath.Join(kernelHome(), "tokens", fmt.Sprintf("%x", h[:6]))
}

func tokenPath() string        { return filepath.Join(tokenDir(), "token") }
func refreshTokenPath() string { return filepath.Join(tokenDir(), "refresh_token") }

func loadToken() (string, error) {
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return "", kernel.ErrUnauthenticated.Wrap("not logged in; run: juice auth login")
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

// promptPassword reads a password from the terminal without echo. It is a
// package var so tests can substitute scripted input for the interactive prompt.
var promptPassword = func(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

// promptNewPassword prompts for a password twice and requires the two entries
// to match, so a mistyped password is caught when it is defined rather than
// silently locking the account. Used wherever a password is set (never on login).
func promptNewPassword(prompt string) (string, error) {
	first, err := promptPassword(prompt)
	if err != nil {
		return "", err
	}
	second, err := promptPassword("Confirm password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", kernel.ErrInvalidInput.Wrap("passwords do not match")
	}
	return first, nil
}

// loadJSONArg resolves a JSON-valued CLI argument to its raw bytes, supporting the
// @path/to/file.json convention (API.md C9): a leading "@" reads the value from the named file.
// Empty input yields "{}". The result is validated as JSON before being returned. This is the
// single loader for every JSON-valued CLI input (run/step-complete args, schemas, auth).
func loadJSONArg(s string) (json.RawMessage, error) {
	if s == "" {
		return json.RawMessage("{}"), nil
	}
	data := []byte(s)
	if strings.HasPrefix(s, "@") {
		b, err := os.ReadFile(s[1:])
		if err != nil {
			return nil, fmt.Errorf("read file %s: %w", s[1:], err)
		}
		data = b
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid JSON")
	}
	return json.RawMessage(data), nil
}

// unmarshalJSONArg loads a JSON-valued argument (see loadJSONArg) and decodes it into v.
func unmarshalJSONArg(s string, v any) error {
	data, err := loadJSONArg(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// readJSONArg loads a JSON-valued argument (see loadJSONArg) and decodes it into an object.
func readJSONArg(s string) (map[string]any, error) {
	data, err := loadJSONArg(s)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return m, nil
}
