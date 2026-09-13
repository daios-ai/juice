// Package main is the CLI entrypoint for the Juice kernel.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/daios-ai/juice/log"
	"github.com/daios-ai/juice/native"
	"github.com/daios-ai/juice/rail"
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
	// worth showing) from a runtime error (usage would be noise). It is also where this
	// invocation's client is made, so every command body has exactly one — and the same one.
	// No subcommand overrides this, so the behavior is uniform across every command.
	PersistentPreRun: func(_ *cobra.Command, _ []string) { enteredCommand, cli = true, &client{} },
	// Hide cobra's stock `completion` command from the help listing (it still works if invoked).
	CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
}

// enteredCommand becomes true once a matched command clears flag/argument validation and its
// body starts. If Execute returns an error while it is still false, the failure came from
// parsing — so main prints usage to show the correct syntax.
var enteredCommand bool

// Global flags.
var (
	flagJSON    bool
	flagQuiet   bool
	flagServer  string
	flagVerbose bool
	flagAs      string
)

// dbPath is where this kernel keeps its ledger, inside its own home (kernelHome). A kernel is
// its directory, so there is no override: initConfig fills this in for the server side alone.
var dbPath string

// globalCfg is populated from the config file before any command runs.
var globalCfg ServerConfig

// resolvedConfigPath is the config file path resolved during initConfig.
var resolvedConfigPath string

func init() {
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "Print the server's JSON reply instead of human-readable text")
	rootCmd.PersistentFlags().BoolVar(&flagQuiet, "quiet", false, "Print only ids, one per line")
	rootCmd.PersistentFlags().StringVar(&flagServer, "server", "", "Server base URL")
	rootCmd.PersistentFlags().BoolVar(&flagVerbose, "verbose", false, "Show underlying error causes")
	rootCmd.PersistentFlags().StringVar(&flagAs, "as", "", "Login to act as for this command, as USER@KERNEL")
}

// juiceHome is the installation root every juice-family program shares: $JUICE_HOME if set, else
// ~/.juice. It holds the kernels this machine runs (kernels/), what this client knows about
// kernels and logins (client/), and each other component's own state — see ecosystem-standard.md.
// It is fixed and absolute — never cwd-relative — so a kernel attaches to the same identity and
// signing key wherever it is launched, like Geth's ~/.ethereum or IPFS's ~/.ipfs. The fallback
// used when the home directory cannot be determined stays absolute (system temp) rather than the
// working directory, preserving that invariant in minimal environments.
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

// cacheDir is the reserved purgeable subdirectory for regenerable data (indexes, compiled
// artifacts, scratch). It is safe to delete; writers MkdirAll it on demand.
func cacheDir() string { return filepath.Join(kernelHome(), "cache") }

// initConfig loads the server's configuration, config.json inside the kernel's own home, beside
// the database it describes. There is no path override: a kernel is its directory, so `juice
// serve` attaches to the same identity and signing key wherever it is launched, instead of minting
// a fresh identity from whatever folder it happens to run in. Only openKernel calls it — no client
// command reads or creates a kernel's directory.
func initConfig() error {
	dbPath = filepath.Join(kernelHome(), "juice.db")
	resolvedConfigPath = filepath.Join(kernelHome(), "config.json")
	// The home holds the config (credentials key), the DB (signing key), and the rail key.
	if err := os.MkdirAll(kernelHome(), 0o700); err != nil {
		return kernel.ErrInvalidState.Wrapf("create %s: %v", kernelHome(), err)
	}
	cfg, err := LoadConfig(resolvedConfigPath)
	if err != nil {
		return kernel.ErrInvalidInput.Wrapf("config: %v", err)
	}
	applyEnvOverrides(&cfg)
	globalCfg = cfg
	return nil
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
	case "terms_changed":
		return 11
	default:
		return 1
	}
}

// openKernel opens the SQLite store and constructs a Kernel from globalCfg.
// The caller is responsible for closing the store when done.
// kernelSecret resolves the JWT secret: the env override (runtime only, §14) else the stored value.
// It never comes from config.json, which is why KernelConfig takes it as an argument.
func kernelSecret(db *store.DB) string {
	if secret := os.Getenv("JUICE_SECRET_KEY"); secret != "" {
		return secret
	}
	stored, _ := db.GetConfig(context.Background(), "jwt_secret")
	return stored
}

func openKernel() (*kernel.Kernel, *store.DB, *log.Logger, *httpActionExecutor, *fedAdapter, []native.Spec, rail.World, error) {
	if err := initConfig(); err != nil {
		return nil, nil, nil, nil, nil, nil, rail.World{}, err
	}
	// Reserve the purgeable cache subdir so the component layout exists for any writer.
	_ = os.MkdirAll(cacheDir(), 0o700)
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, rail.World{}, fmt.Errorf("open db: %w", err)
	}

	cfg, err := globalCfg.KernelConfig(kernelSecret(db))
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, rail.World{}, err
	}

	logger, err := log.New(log.Config{
		Level:    globalCfg.LogLevel,
		FilePath: globalCfg.LogFile,
		Format:   globalCfg.LogFormat,
	})
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, rail.World{}, fmt.Errorf("log_file %s: %w", globalCfg.LogFile, err)
	}

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

	// The world this kernel serves fixes its network, whose digest binds every signature it makes
	// and the namespace it discovers on (D23). A kernel that already has one is not asked again:
	// the database is where that answer lives.
	world, err := worldFor(context.Background(), db, globalCfg.World, resolvedConfigPath)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, rail.World{}, err
	}
	cfg.Network = world.Network()
	// Every money rule comes from one place, the operator's own configuration (P10).
	econ, err := globalCfg.Economy()
	if err != nil {
		return nil, nil, nil, nil, nil, nil, rail.World{}, err
	}

	httpExec := &httpActionExecutor{timeout: cfg.ScriptTimeout, allowLocal: cfg.AllowLocalSources}
	k := kernel.New(kernel.Dependencies{
		Store:    db,
		Scripts:  exec,
		HTTP:     httpExec, // one cohesive HTTP concern: dispatch + ordinary fetching
		Embedder: embedder,
		Config:   cfg,
		Economy:  econ,
		Logger:   logger,
	})
	// Federation is a separate adapter, constructed around the kernel's own signer and attached
	// after (§13): it holds neither the kernel nor the private key, and its transport arrives at
	// serve time via SetTransport. Signing goes live when bootstrap calls SetSigningKey.
	fedAdapter := newFedAdapter("", k.SignFederation, k.RailIdentity)
	k.SetFederation(fedAdapter)

	// Credential encryption. The key is minted at first boot and only read here; one that cannot be
	// used is refused rather than dropped, since a server without it answers every credentialed call
	// with "could not be decrypted" and names nothing an operator can act on.
	keyBytes, err := base64.RawURLEncoding.DecodeString(globalCfg.CredentialsKey)
	if err != nil || len(keyBytes) != 32 {
		db.Close()
		return nil, nil, nil, nil, nil, nil, rail.World{}, fmt.Errorf(
			"credentials_key in %s is not a 32-byte base64url key; it seals every stored credential, so restore it from your backup", resolvedConfigPath)
	}
	box, err := newAESGCMBox(keyBytes)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, rail.World{}, err
	}
	k.SetSecretBox(box)
	// The §9 authenticator shares the box (to open sealed auth configs and grant tokens)
	// and reads/rotates grants through the store (§8).
	httpExec.auth = newAuthenticator(box, db, cfg.AllowLocalSources, cfg.ScriptTimeout)
	httpExec.auth.refFn = k.ActionRef // qualified @owner/name in grant-required errors

	// Register the platform stdlib. Each native declares its own contract (native.Spec), so this
	// wiring names adapters only — never a schema or description. Must happen on every kernel open,
	// not just bootstrap.
	webUA := "juice-kernel/" + version + " (+https://github.com/daios-ai/juice)"
	nativeDeps := native.Deps{
		Chatter:  chatter,
		Embedder: embedder,
		JSON:     ollamaChatter,
		Decide:   ollamaChatter,
		Web: native.WebDeps{Fetch: func(ctx context.Context, url string) (int, []byte, string, string, error) {
			return httpExec.fetchWeb(ctx, url, webUA)
		}},
		Compile:    native.CompileDeps{Compiler: script.NewTinyGoCompiler(script.CompileConfig{}), Scripts: exec},
		CompileSDK: script.TinyGoSDK,
	}
	specs := native.All(nativeDeps)
	native.Register(k, specs)

	// Load signing key if present (best-effort; no error if not yet bootstrapped).
	if privB64, _ := db.GetConfig(context.Background(), configKeySigningPrivate); privB64 != "" {
		if privBytes, err := base64.RawURLEncoding.DecodeString(privB64); err == nil && len(privBytes) == ed25519.PrivateKeySize {
			if su, err := db.ReadUserByHandle(context.Background(), "sys"); err == nil {
				k.SetSigningKey(ed25519.PrivateKey(privBytes), su.ID)
			}
		}
	}

	// The adapter learns this kernel's own public key once bootstrap has loaded the signing key.
	if pub, _ := db.GetConfig(context.Background(), configKeySigningPublic); pub != "" {
		fedAdapter.SetLocalPubKey(pub)
	}

	return k, db, logger, httpExec, fedAdapter, specs, world, nil
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
