package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

// A client names a kernel and a principal separately, and joins them in a context — the shape
// kubeconfig settled on, for the same reason: one kernel serves many principals, one principal is
// reached from many machines, and the two facts change independently. A context is what every
// program in the installation names, so "which kernel does this component use" is answered by a
// file rather than by whatever happens to answer a port.
//
// Credentials do not live here. They belong to one context's login session and sit in their own
// file, so two programs sharing an installation never share a token, and rotating one login never
// rewrites another's (see credentials).

// defaultEndpoint is where a kernel listens when nothing says otherwise.
const defaultEndpoint = "http://localhost:4040"

// defaultContext names the context a client addresses when it has been told no other. It is the
// client's own label for "the one I use", and has nothing to do with any kernel's directory.
const defaultContext = "default"

// kernelRec is one kernel this client knows: where it answers, and the identity it reported there.
// The key and network are what `juice use` checks before switching, so a command never lands on a
// kernel other than the one this record was made for.
type kernelRec struct {
	Endpoint    string `json:"endpoint"`
	PublicKey   string `json:"public_key,omitempty"`
	WorldDigest string `json:"world_digest,omitempty"`
	Network     string `json:"network,omitempty"`
	Decimals    uint8  `json:"decimals,omitempty"`
}

// contextRec joins one kernel to one login session on it. Handle and principal id are recorded at
// login: the handle to show, the id because it is what stays stable when a handle is renamed, and
// so is what other components key their own memory of this kernel by (D15).
type contextRec struct {
	Kernel      string `json:"kernel"`
	Handle      string `json:"handle,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
}

type clientConfig struct {
	Current  string                 `json:"current"`
	Kernels  map[string]*kernelRec  `json:"kernels"`
	Contexts map[string]*contextRec `json:"contexts"`
}

// clientHome is the client's own subdirectory, $JUICE_HOME/client. Kernel directories sit beside
// it and are never read here: every command is a TCP client (API.md C13).
func clientHome() string { return filepath.Join(juiceHome(), "client") }

func clientConfigPath() string { return filepath.Join(clientHome(), "config.json") }

func credentialsDir() string { return filepath.Join(clientHome(), "credentials") }

// credentialPath is where a name becomes a path, and the only place it does, so it is where a name
// that is not a name is refused. A context is one file under credentials/; a label carrying a
// separator or a leading dot would address something else entirely — client/config.json, say — and
// the write that follows would overwrite it with a credential blob.
func credentialPath(ctxName string) (string, error) {
	if err := validateLocalName("context", ctxName); err != nil {
		return "", err
	}
	return filepath.Join(credentialsDir(), ctxName+".json"), nil
}

// loadClientConfig reads the client's records, migrating a pre-context profiles.json the first
// time it finds one. An unreadable file yields empty records rather than an error: a client that
// cannot parse its own notes must still be able to reach a server and log in again.
func loadClientConfig() *clientConfig {
	cfg := &clientConfig{}
	data, err := os.ReadFile(clientConfigPath())
	if err != nil {
		if migrated := migrateLegacyProfiles(); migrated != nil {
			cfg = migrated
		}
	} else {
		_ = json.Unmarshal(data, cfg)
	}
	if cfg.Kernels == nil {
		cfg.Kernels = map[string]*kernelRec{}
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*contextRec{}
	}
	if cfg.Current == "" {
		cfg.Current = defaultContext
	}
	return cfg
}

// saveClientConfig writes the records atomically — temp file in the same directory, then rename —
// so an interrupted write never leaves a client without its bearings. The file holds no secret;
// credentials are their own files.
func saveClientConfig(cfg *clientConfig) error {
	if err := os.MkdirAll(clientHome(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(clientHome(), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), clientConfigPath())
}

// activeContextName is the context this invocation addresses: --context, else JUICE_CONTEXT, else
// the recorded current one. The first two never write, so selecting a context for one command
// cannot move an agent or an open interface onto another kernel.
func activeContextName(cfg *clientConfig) string {
	if n := strings.TrimSpace(flagContext); n != "" {
		return n
	}
	if n := strings.TrimSpace(os.Getenv("JUICE_CONTEXT")); n != "" {
		return n
	}
	return cfg.Current
}

// activeContext returns the records this invocation addresses. A name with no entry yet gets one
// in memory, so an unconfigured client still reaches the local kernel; nothing is written until
// something is stored in it.
func activeContext() (*clientConfig, string, *contextRec, *kernelRec) {
	cfg := loadClientConfig()
	name := activeContextName(cfg)
	c := cfg.Contexts[name]
	if c == nil {
		c = &contextRec{Kernel: name}
		cfg.Contexts[name] = c
	}
	if c.Kernel == "" {
		c.Kernel = name
	}
	k := cfg.Kernels[c.Kernel]
	if k == nil {
		k = &kernelRec{}
		cfg.Kernels[c.Kernel] = k
	}
	if k.Endpoint == "" {
		k.Endpoint = defaultEndpoint
	}
	return cfg, name, c, k
}

// ---- credentials ----

// credentials is one login session: the tokens a context holds. It is its own file, 0600, named
// for the context, because a session's lifecycle is its own — the interface, an agent and the
// command line may all authenticate as one principal without sharing a refresh token, and
// rotating one must not silently invalidate another.
type credentials struct {
	Token        string `json:"token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// withCredentials opens the addressed context's credential file under an exclusive lock and hands
// it to fn. The lock is what makes successive and concurrent processes on one session safe: a
// refresh is a read, a round trip and a write, and two of them interleaved would leave one holding
// a rotated-away token. Everything fn needs to decide is inside the lock, so it can see that
// another process refreshed first and simply use what that one stored.
func withCredentials(fn func(*credentials) (bool, error)) error {
	if err := os.MkdirAll(credentialsDir(), 0o700); err != nil {
		return err
	}
	_, name, _, _ := activeContext()
	path, err := credentialPath(name)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var c credentials
	if data, rerr := os.ReadFile(path); rerr == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &c)
	}
	write, err := fn(&c)
	if err != nil || !write {
		return err
	}
	data, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// readCredentials reads the addressed context's tokens without holding the lock across anything
// else. Every caller that also writes goes through withCredentials instead.
func readCredentials() credentials {
	var c credentials
	_, name, _, _ := activeContext()
	path, err := credentialPath(name)
	if err != nil {
		return c
	}
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

func loadToken() (string, error) {
	c := readCredentials()
	if c.Token == "" {
		return "", kernel.ErrUnauthenticated.Wrap("not logged in; run: juice auth login")
	}
	return c.Token, nil
}

// saveToken stores the access token for the addressed context. A context whose kernel has no
// recorded identity yet also adopts the address the token came from, so that token travels back
// only there.
func saveToken(tok string) error {
	cfg, _, _, k := activeContext()
	if k.PublicKey == "" && k.Endpoint != serverBaseURL() {
		k.Endpoint = serverBaseURL()
		if err := saveClientConfig(cfg); err != nil {
			return err
		}
	}
	return withCredentials(func(c *credentials) (bool, error) {
		c.Token = tok
		return true, nil
	})
}

func removeToken() error {
	return withCredentials(func(c *credentials) (bool, error) { c.Token = ""; return true, nil })
}

func loadRefreshToken() (string, error) {
	c := readCredentials()
	if c.RefreshToken == "" {
		return "", os.ErrNotExist
	}
	return c.RefreshToken, nil
}

func saveRefreshToken(tok string) error {
	return withCredentials(func(c *credentials) (bool, error) { c.RefreshToken = tok; return true, nil })
}

func removeRefreshToken() error {
	return withCredentials(func(c *credentials) (bool, error) { c.RefreshToken = ""; return true, nil })
}

// ---- releasing a credential ----

// atHome reports whether base is the addressed context's own kernel address — the one place its
// credentials may go. Nothing a server says about itself can earn it a credential: an unsigned
// banner is a claim, not a proof, so any other address gets every request anonymously.
func atHome(base string) bool {
	_, _, _, k := activeContext()
	return strings.TrimRight(base, "/") == strings.TrimRight(k.Endpoint, "/")
}

// tokenFor returns the bearer token to send to base, or why none is sent.
func tokenFor(base string) (string, error) {
	_, name, _, _ := activeContext()
	c := readCredentials()
	if c.Token == "" {
		return "", kernel.ErrUnauthenticated.Wrap("not logged in; run: juice auth login")
	}
	if !atHome(base) {
		return "", kernel.ErrUnauthenticated.Wrapf(
			"%s is not the kernel you are logged in to (context %s), so the request was sent without your login", base, name)
	}
	return c.Token, nil
}

// probeHealth reads a server's identity banner at most once per address per run: deciding what a
// client may do must not multiply the requests a command makes.
var (
	healthMu    sync.Mutex
	healthCache = map[string]*serverHealth{}
)

func probeHealth(ctx context.Context, base string) (*serverHealth, error) {
	healthMu.Lock()
	defer healthMu.Unlock()
	if h, ok := healthCache[base]; ok {
		if h == nil {
			return nil, kernel.ErrPeerUnreachable.Wrapf("cannot reach %s", base)
		}
		return h, nil
	}
	h, err := health(ctx, base)
	healthCache[base] = h // a failure is cached as nil, so one unreachable server is dialed once
	if err != nil {
		healthCache[base] = nil
		return nil, err
	}
	return h, nil
}

// ---- amounts ----

// parseAmount converts an amount as a person writes it into the whole base units the kernel counts
// in. The digits are shifted by hand: money never passes through floating point.
func parseAmount(s string, decimals uint8) (int64, error) {
	bad := kernel.ErrInvalidInput.Wrap("amount must be a positive whole number")
	if decimals > 0 {
		bad = kernel.ErrInvalidInput.Wrapf("amount must be positive, with at most %d decimal places", decimals)
	}
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if !allDigits(whole) || (frac != "" && !allDigits(frac)) || len(frac) > int(decimals) {
		return 0, bad
	}
	v, err := strconv.ParseInt(whole+frac+strings.Repeat("0", int(decimals)-len(frac)), 10, 64)
	if err != nil || v <= 0 {
		return 0, bad
	}
	return v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- migration from the pre-context layout ----

// migrateLegacyProfiles converts a profiles.json — one record that was a kernel, a login and a
// selection at once — into the three it is now. Each profile becomes a kernel, a context on it and
// that context's credential file; the old active becomes current. The old file is kept, renamed,
// so nothing is destroyed by a client that ran once. It returns nil when there is nothing to migrate.
func migrateLegacyProfiles() *clientConfig {
	legacy := filepath.Join(clientHome(), "profiles.json")
	data, err := os.ReadFile(legacy)
	if err != nil {
		return nil
	}
	var old struct {
		Active   string `json:"active"`
		Profiles map[string]struct {
			Endpoint     string `json:"endpoint"`
			PublicKey    string `json:"public_key"`
			WorldDigest  string `json:"world_digest"`
			Network      string `json:"network"`
			Decimals     uint8  `json:"decimals"`
			Token        string `json:"token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"profiles"`
	}
	if json.Unmarshal(data, &old) != nil {
		return nil
	}
	cfg := &clientConfig{
		Current:  old.Active,
		Kernels:  map[string]*kernelRec{},
		Contexts: map[string]*contextRec{},
	}
	for name, p := range old.Profiles {
		cfg.Kernels[name] = &kernelRec{
			Endpoint:    p.Endpoint,
			PublicKey:   p.PublicKey,
			WorldDigest: p.WorldDigest,
			Network:     p.Network,
			Decimals:    p.Decimals,
		}
		cfg.Contexts[name] = &contextRec{Kernel: name}
		if p.Token == "" && p.RefreshToken == "" {
			continue
		}
		path, perr := credentialPath(name)
		if perr != nil {
			continue // a profile whose name cannot be a file keeps its records and loses its login
		}
		if err := os.MkdirAll(credentialsDir(), 0o700); err != nil {
			return nil
		}
		blob, _ := json.MarshalIndent(&credentials{Token: p.Token, RefreshToken: p.RefreshToken}, "", "  ")
		_ = os.WriteFile(path, append(blob, '\n'), 0o600)
	}
	if cfg.Current == "" {
		cfg.Current = defaultContext
	}
	if saveClientConfig(cfg) != nil {
		return nil
	}
	_ = os.Rename(legacy, legacy+".migrated")
	fmt.Fprintf(os.Stderr, "moved client profiles into %s (old file kept as %s.migrated)\n", clientConfigPath(), legacy)
	return cfg
}

// ---- use ----

func init() { rootCmd.AddCommand(useCmd()) }

func useCmd() *cobra.Command {
	var endpoint, kernelName string
	cmd := &cobra.Command{
		Use:   "use [NAME]",
		Short: "Switch between the kernels and logins this client knows",
		Long: "Switch between the kernels and logins this client knows. A context is a name for one\n" +
			"kernel and one login on it: the address that kernel answers on, the key it must present,\n" +
			"and your session there.\n\n" +
			"With no arguments, lists the contexts and marks the one in use. With NAME, switches to\n" +
			"that context, refusing it if the server no longer presents the key and network the\n" +
			"context recorded. With --endpoint, adds NAME (or points it somewhere else) and records\n" +
			"the identity that server presents. With --kernel, adds NAME as a second login on a\n" +
			"kernel this client already knows.\n\n" +
			"JUICE_CONTEXT=NAME or --context NAME selects a context for a single command without\n" +
			"switching, which is how an unattended program names the kernel it works on.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				if endpoint != "" || kernelName != "" {
					return kernel.ErrInvalidInput.Wrap("name the context that address belongs to")
				}
				return listContexts()
			}
			if endpoint != "" && kernelName != "" {
				return kernel.ErrInvalidInput.Wrap("give either --endpoint or --kernel, not both")
			}
			return useContext(context.Background(), args[0], endpoint, kernelName)
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Address this context's kernel answers on, e.g. http://localhost:4040")
	cmd.Flags().StringVar(&kernelName, "kernel", "", "Name of a kernel this client already knows, to hold a second login on")
	return cmd
}

func listContexts() error {
	cfg := loadClientConfig()
	active := activeContextName(cfg)
	if flagJSON {
		return printJSON(cfg)
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mark := " "
		if name == active {
			mark = "*"
		}
		c := cfg.Contexts[name]
		k := cfg.Kernels[c.Kernel]
		if k == nil {
			k = &kernelRec{}
		}
		who := c.Handle
		if who == "" {
			who = "—"
		}
		fmt.Printf("%s %-16s %-16s %-32s %s\n", mark, name, who, k.Endpoint, k.Network)
	}
	return nil
}

// useContext switches to a context, verifying it first. Without --endpoint the recorded identity is
// a promise the server must still keep; with it, the operator is naming a kernel for the first time
// and what that server presents is what gets recorded — the one moment an unknown key is believed,
// which is why it is an explicit act and not a side effect of any other command.
//
// Nothing is written until the address has answered. A mistyped one is a refusal and no more: the
// records stay as they were and the logins on that kernel survive, because a client left pointing
// at its old kernel with its sessions destroyed would be describing a state that never existed.
func useContext(ctx context.Context, name, endpoint, kernelName string) error {
	if err := validateLocalName("context", name); err != nil {
		return err
	}
	cfg := loadClientConfig()
	c := cfg.Contexts[name]
	if c == nil {
		if endpoint == "" && kernelName == "" {
			return kernel.ErrNotFound.Wrapf(
				"no context named %s; add it with: juice use %s --endpoint URL", name, name)
		}
		c = &contextRec{Kernel: name}
	}
	kName := c.Kernel
	if kernelName != "" {
		if cfg.Kernels[kernelName] == nil {
			return kernel.ErrNotFound.Wrapf("no kernel named %s; add it with: juice use %s --endpoint URL", kernelName, kernelName)
		}
		kName = kernelName
	}
	if kName == "" {
		kName = name
	}
	// Decide what to dial without touching what is recorded. `k` is a copy: it becomes the record
	// only once the server at the other end has answered as the kernel this context expects.
	k := &kernelRec{}
	if existing := cfg.Kernels[kName]; existing != nil {
		copyOf := *existing
		k = &copyOf
	}
	repointed := endpoint != ""
	if repointed {
		// A login belongs to the address it was made at: whatever answers at the new one starts
		// with no credentials, and every login on this kernel is stranded — once it answers.
		k.Endpoint = strings.TrimRight(endpoint, "/")
		k.PublicKey = ""
	}
	if k.Endpoint == "" {
		k.Endpoint = defaultEndpoint
	}
	h, err := health(ctx, k.Endpoint)
	if err != nil {
		return err
	}
	if !repointed {
		if k.PublicKey != "" && h.PublicKey != k.PublicKey {
			return kernel.ErrInvalidState.Wrapf(
				"the server at %s is a different kernel than context %s recorded; not switching", k.Endpoint, name)
		}
		if k.WorldDigest != "" && h.Digest != k.WorldDigest {
			return kernel.ErrInvalidState.Wrapf(
				"the server at %s now serves the %s network, not the one context %s recorded; not switching", k.Endpoint, h.Network, name)
		}
	}
	k.PublicKey, k.WorldDigest, k.Network, k.Decimals = h.PublicKey, h.Digest, h.Network, h.Decimals
	// Past every refusal: commit the records, and only now strand the logins a repoint invalidates.
	cfg.Contexts[name] = c
	c.Kernel = kName
	cfg.Kernels[kName] = k
	if repointed {
		for ctxName, other := range cfg.Contexts {
			if other.Kernel != kName {
				continue
			}
			if path, perr := credentialPath(ctxName); perr == nil {
				_ = os.Remove(path)
			}
			other.Handle, other.PrincipalID = "", ""
		}
	}
	cfg.Current = name
	if err := saveClientConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("Using %s\n  address: %s\n  kernel:  %s\n  network: %s\n", name, k.Endpoint, k.PublicKey, k.Network)
	return nil
}
