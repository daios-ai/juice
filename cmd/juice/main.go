// SPDX-License-Identifier: AGPL-3.0-only

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
	"strconv"
	"strings"
	"unicode/utf8"

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
	Use:   "juice",
	Short: "Juice kernel — callable action platform",
	// Two examples, because the first screen of a program nobody has used before should say where
	// to start: run a kernel of your own, or use somebody else's (§14).
	Example: "  # run a kernel of your own, and use it\n" +
		"  juice kernel serve acme\n" +
		"  juice auth login sys@acme\n\n" +
		"  # use a kernel somebody else runs\n" +
		"  juice kernel add https://their.example acme\n" +
		"  juice user create me@acme",
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
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		enteredCommand, cli = true, &client{}
		return checkGlobalFlags(cmd)
	},
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
	cobra.AddTemplateFuncs(map[string]any{
		"wrap":        func(s string) string { return wrapText(s, helpWidth(), 0) },
		"wrapCommand": func(c *cobra.Command) string { return wrapText(c.Short, helpWidth(), 3+c.NamePadding()) },
		"wrapFlags":   func(f interface{ FlagUsagesWrapped(int) string }) string { return f.FlagUsagesWrapped(helpWidth()) },
	})
	rootCmd.SetHelpTemplate(helpTemplate)
	rootCmd.SetUsageTemplate(usageTemplate)
}

// maxHelpWidth is the column help is written for, the conventional terminal width.
const maxHelpWidth = 80

// helpWidth is the column help wraps at: the terminal's width, capped at maxHelpWidth, since long
// lines read worse even on a wide screen; maxHelpWidth when there is no terminal. Help is printed on
// stdout and the usage after a mistyped command on stderr, so either being a terminal is where the
// text will be read.
var helpWidth = func() int { return widthOf(term.GetSize) }

// widthOf is helpWidth's rule over a terminal-size query, so the rule is testable without a terminal.
func widthOf(size func(fd int) (width, height int, err error)) int {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if w, _, err := size(int(f.Fd())); err == nil && w > 0 {
			return min(w, maxHelpWidth)
		}
	}
	return maxHelpWidth
}

// wrapText fills s to width columns, word by word, counting runes. Its first line is taken to start
// at column indent, where the caller has already written the text before it, and every later line is
// indented to match. A blank line or one starting with a space (a list, an aligned table) is kept as
// written; a word longer than the room left stays whole on a line of its own.
func wrapText(s string, width, indent int) string {
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent))
		}
		if line == "" || line[0] == ' ' {
			b.WriteString(line)
			continue
		}
		col := indent
		for j, word := range strings.Fields(line) {
			n := utf8.RuneCountInString(word)
			if j > 0 && col+1+n > width {
				b.WriteString("\n" + strings.Repeat(" ", indent))
				col = indent
			} else if j > 0 {
				b.WriteString(" ")
				col++
			}
			b.WriteString(word)
			col += n
		}
	}
	return b.String()
}

// helpTemplate and usageTemplate are Cobra v1.10.2's defaults with only the wrapping added: the
// description, each command's summary, the flag lists, and the closing hint wrap at helpWidth.
// Usage lines and examples stay as written, since a wrapped command no longer pastes.
const helpTemplate = `{{with (or .Long .Short)}}{{wrap . | trimTrailingWhitespaces}}

{{end}}{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`

const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{wrapCommand .}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{wrapCommand .}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{wrapCommand .}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{wrapFlags .LocalFlags | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{wrapFlags .InheritedFlags | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

{{wrap (printf "Use \"%s [command] --help\" for more information about a command." .CommandPath)}}{{end}}
`

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
	// Last word to the command line, over both the file and the environment (§14).
	applyConfigFlags(&cfg)
	globalCfg = cfg
	return nil
}

func main() {
	// ExecuteC returns the command that actually ran/failed, so usage (when shown) is that
	// command's, not the root's.
	cmd, err := rootCmd.ExecuteC()
	if err != nil {
		if !enteredCommand {
			err = inputError(cmd, err)
		}
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
	// A declined confirmation is the answer the prompt asked for, not a failure: one word, no
	// colour, no "error:" — and the non-zero exit above still tells a script what happened.
	if errors.Is(err, errCancelled) {
		fmt.Fprintln(os.Stderr, "cancelled")
		return
	}
	fmt.Fprintln(os.Stderr, errorLine(err.Error(), useColor()))
	if r := remedy(err); r != "" {
		fmt.Fprintln(os.Stderr, "       "+r)
	}
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

func openKernel(world rail.World) (*kernel.Kernel, *store.DB, *log.Logger, *httpActionExecutor, *fedAdapter, []native.Spec, error) {
	if err := initConfig(); err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	// Reserve the purgeable cache subdir so the component layout exists for any writer.
	_ = os.MkdirAll(cacheDir(), 0o700)
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("open db: %w", err)
	}

	cfg, err := globalCfg.KernelConfig(kernelSecret(db))
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, err
	}

	logger, err := log.New(log.Config{
		Level:    globalCfg.LogLevel,
		FilePath: globalCfg.LogFile,
		Format:   globalCfg.LogFormat,
	})
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("log_file %s: %w", globalCfg.LogFile, err)
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

	// The world this kernel serves fixes its network, whose fingerprint binds every signature it
	// makes and the namespace it discovers on (D23). A database made on another network is refused
	// here, before the rail is opened, so a kernel served from the wrong world dials nothing.
	if err := checkNetwork(context.Background(), db, world); err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, err
	}
	cfg.Network = world.Network()
	// Every money rule comes from one place, the operator's own configuration (P10).
	econ, err := globalCfg.Economy()
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
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
	fedAdapter := newFedAdapter("", k.SignFederation, k.BlockchainIdentity)
	k.SetFederation(fedAdapter)

	// Credential encryption. The key is minted at first boot and only read here; one that cannot be
	// used is refused rather than dropped, since a server without it answers every credentialed call
	// with "could not be decrypted" and names nothing an operator can act on.
	keyBytes, err := base64.RawURLEncoding.DecodeString(globalCfg.CredentialsKey)
	if err != nil || len(keyBytes) != 32 {
		db.Close()
		return nil, nil, nil, nil, nil, nil, fmt.Errorf(
			"credentials_key in %s is not a 32-byte base64url key; it seals every stored credential, so restore it from your backup", resolvedConfigPath)
	}
	box, err := newAESGCMBox(keyBytes)
	if err != nil {
		db.Close()
		return nil, nil, nil, nil, nil, nil, err
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

	return k, db, logger, httpExec, fedAdapter, specs, nil
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

// remedy is the second half of every error a person reads: what to do about it. It is written here
// and nowhere else, from the error's own code and the structured fields the kernel attached to it,
// so one failure reads the same whichever command met it — and a command never prints a paragraph
// of its own beside the error line (§14). An error with nothing useful to add says nothing.
func remedy(err error) string {
	ke := (*kernel.KernelError)(nil)
	if !errors.As(err, &ke) {
		return ""
	}
	peer := ke.Meta["peer"]
	switch {
	// Not allowed as you are: name the one command that changes that.
	case errors.Is(err, kernel.ErrGrantRequired):
		if ref := ke.Meta["action"]; ref != "" {
			return "Authorize it with: juice user connect " + directorySelector(ref)
		}
	// Cannot be done now, and nothing moved. The condition is the peer's, and only its operator
	// can change it, so the buyer is told what happened and not sent to inspect their own books.
	case errors.Is(err, kernel.ErrPeerUnreachable):
		switch {
		case ke.Meta["world"] != "":
			return "Start it with: juice kernel serve " + ke.Meta["world"]
		case peer != "":
			return "Nothing was charged. Try again when " + peer + " is back."
		case ke.Meta["kernel"] != "":
			return "Try again when " + ke.Meta["kernel"] + " is answering."
		}
	case errors.Is(err, kernel.ErrPeerUnfunded):
		if peer != "" {
			return peer + " declined to serve this call; nothing was charged. Only that kernel's operator can change it."
		}
		return "The peer declined to serve this call; nothing was charged."
	case errors.Is(err, kernel.ErrTermsChanged):
		if h := ke.Meta["quote_hash"]; h != "" {
			return fmt.Sprintf("Nothing was charged. The price is now %s; pass --quote-hash %s to accept it.", ke.Meta["price"], h)
		}
	// A parked call: the money is reserved, not spent, and the process is the handle to follow it by.
	case ke.Meta["process_id"] != "":
		id := ke.Meta["process_id"]
		if at := ke.Meta["pending_since"]; at != "" {
			return fmt.Sprintf("Your funds are reserved, not spent, on process %s, waiting for the peer's answer since %s. It retries by itself; follow it with: juice process show %s", id, at, id)
		}
		return fmt.Sprintf("Your funds are reserved, not spent, on process %s. Follow it with: juice process show %s", id, id)
	// It ran and failed: what it drew, and where the record of it is.
	case ke.Meta["tx_id"] != "":
		return fmt.Sprintf("Charged %s. The record is: juice tx show %s", renderCharge(ke.Meta["charge"]), ke.Meta["tx_id"])
	case errors.Is(err, kernel.ErrInternal):
		return "This is a fault in juice. Re-run with --verbose for the detail behind it."
	}
	return ""
}

// renderCharge writes what a failed call drew the way every other amount is written. The unit is
// this client's already-read record of the kernel it spoke to, so reporting a charge never costs a
// request — and an unreadable one says the base-unit number rather than nothing.
func renderCharge(raw string) string {
	amount, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return raw
	}
	net, nerr := humanUnits(context.Background())
	if nerr != nil {
		return raw
	}
	return net.Amount(amount)
}

// inputError is what a mistake in the command line says. Only the wrong number of arguments is
// reworded — cobra counts them and says so in its own words ("accepts between 1 and 2 arg(s),
// received 0"), where this program can name what the command takes. Everything else it reports —
// an unknown command, an unknown flag, a flag given no value — is already about the words the
// person typed, so it is carried through as written and only given a code (§14).
func inputError(cmd *cobra.Command, err error) error {
	if strings.HasPrefix(err.Error(), "accepts ") || strings.HasPrefix(err.Error(), "requires ") ||
		strings.Contains(err.Error(), "arg(s)") {
		return kernel.ErrInvalidInput.Wrapf("%s takes %s", cmd.CommandPath(), argumentsOf(cmd))
	}
	return kernel.ErrInvalidInput.Wrap(err.Error())
}

// argumentsOf is what a command's own Use line says it takes, which is the part after its name:
// `run ACTION [JSON]` takes `ACTION [JSON]`. A command that takes nothing says so.
func argumentsOf(cmd *cobra.Command) string {
	_, args, _ := strings.Cut(cmd.Use, " ")
	if strings.TrimSpace(args) == "" {
		return "no arguments"
	}
	return strings.TrimSpace(args)
}

// checkGlobalFlags refuses the two ways of asking for something no command can answer: two output
// formats at once, and acting as a login on a command that acts as nobody. A flag that is accepted
// and ignored teaches that it works (§14).
func checkGlobalFlags(cmd *cobra.Command) error {
	if flagJSON && flagQuiet {
		return kernel.ErrInvalidInput.Wrap("--json and --quiet are two answers to one question; pass one")
	}
	if flagAs != "" && !actsAsALogin(cmd) {
		return kernel.ErrInvalidInput.Wrapf("%s acts on this client's own records, not as a login, so --as means nothing here", cmd.CommandPath())
	}
	return nil
}

// actsAsALogin reports whether a command acts as somebody on a kernel. Two whole nouns do not:
// `kernel`, which is this client's address book and the server itself, and `auth`, which is the
// logins themselves rather than anything done as one. `user create` is the third, since the login
// it makes is the one it names. Taken from the command's place in the tree rather than from a list
// of names, so a command added under either noun is covered the day it is added.
func actsAsALogin(cmd *cobra.Command) bool {
	if cmd.CommandPath() == "juice user create" {
		return false
	}
	for c := cmd; c != nil && c.Parent() != nil; c = c.Parent() {
		if c.Name() != "kernel" && c.Name() != "auth" {
			continue
		}
		// `kernel` and `auth` at the top are this client's own; under `admin` they are the
		// operator's commands about a kernel, which do act as a login.
		return c.Parent().Name() == "admin"
	}
	return true
}
