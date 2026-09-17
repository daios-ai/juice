// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/rail"
)

// NativeLLMConfig holds configuration for the @sys/llm/* native actions.
type NativeLLMConfig struct {
	URL        string `json:"url"`
	ChatModel  string `json:"chat_model"`
	EmbedModel string `json:"embed_model"`
	Price      int64  `json:"price"`
}

// NativeLookupConfig holds configuration for the @sys/lookup native action.
type NativeLookupConfig struct {
	DefaultLimit int   `json:"default_limit"`
	Price        int64 `json:"price"`
}

// NativeWebConfig holds configuration for the @sys/web native action. The User-Agent it sends is
// derived from the binary's own version, not configured: it identifies the software making the
// request, which is a fact about the build rather than an operator preference.
type NativeWebConfig struct {
	Price int64 `json:"price"`
}

// NativePriceConfig is the whole configuration of a native whose only setting is its price —
// most of the stdlib. One type instead of one struct per action; the JSON shape is unchanged (§14).
type NativePriceConfig struct {
	Price int64 `json:"price"`
}

// NativeConfig holds per-action configuration for all native actions (§14 `native.<action>`).
// Configuration owns prices and their defaults; each native's contract lives with its handler (§9).
type NativeConfig struct {
	LLM      NativeLLMConfig    `json:"llm"`
	Lookup   NativeLookupConfig `json:"lookup"`
	Time     NativePriceConfig  `json:"time"`
	Sink     NativePriceConfig  `json:"sink"`
	Message  NativePriceConfig  `json:"message"`
	Random   NativePriceConfig  `json:"random"`
	Web      NativeWebConfig    `json:"web"`
	TinyGo   NativePriceConfig  `json:"tinygo"`
	Transfer NativePriceConfig  `json:"transfer"`
}

// PriceOf returns the configured price for a native action name (§9 names, §14 config keys). It is
// the single place the two vocabularies meet, so bootstrap never restates either.
func (c NativeConfig) PriceOf(name string) int64 {
	switch name {
	case "lookup":
		return c.Lookup.Price
	case "llm/chat", "llm/embed", "llm/json", "llm/decide":
		return c.LLM.Price
	case "time":
		return c.Time.Price
	case "sink":
		return c.Sink.Price
	case "message":
		return c.Message.Price
	case "random":
		return c.Random.Price
	case "transfer":
		return c.Transfer.Price
	case "web":
		return c.Web.Price
	case "tinygo/compile":
		return c.TinyGo.Price
	}
	return 0
}

// ServerConfig holds all non-secret runtime configuration.
// Secrets (JUICE_SECRET_KEY, JUICE_BOOTSTRAP_PASSWORD) are read from environment variables.
// All other settings come from this struct, populated from the JSON config file.
type ServerConfig struct {
	Native                     NativeConfig `json:"native"`
	ScriptTimeoutMS            int64        `json:"script_timeout_ms"`
	ScriptMemoryBytes          int64        `json:"script_memory_bytes"`
	FeeBPS                     int64        `json:"fee_bps"`
	RemoteBPS                  int64        `json:"remote_bps"`   // serving-side markup on inbound remote calls (§13)
	ImportBPS                  int64        `json:"import_bps"`   // origin-side import fee on outbound remote calls, retained locally (§13)
	Lottery                    *int64       `json:"lottery"`      // L: the ticket this kernel writes (P10); 0 = pay every obligation exactly
	LotteryMax                 *int64       `json:"lottery_max"`  // the largest ticket this kernel accepts from a buyer (P10)
	CreditLimit                *int64       `json:"credit_limit"` // E_max: most unpaid delivered service carried at once (P10)
	TokenTTL                   string       `json:"token_ttl"`
	AuthIssuer                 string       `json:"auth_issuer"`
	AuthAudience               string       `json:"auth_audience"`
	LogLevel                   string       `json:"log_level"`
	LogFile                    string       `json:"log_file"`
	LogFormat                  string       `json:"log_format"`
	AllowLocalSources          bool         `json:"allow_local_sources"`
	ListenAddr                 string       `json:"listen_addr"`                   // address the client API binds, host:port; host omitted ⇒ every interface, port 0 ⇒ OS-assigned
	HTTPCallbackURL            string       `json:"http_callback_url"`             // base URL advertised to dispatched kind=http endpoints for capability callbacks (§9); "" ⇒ derive from listen address
	KernelHandle               string       `json:"kernel_handle"`                 // handle this kernel presents in gossip (§13)
	World                      string       `json:"world"`                         // the network this kernel serves: play, test, real, or a world file's path (D23)
	RailRPC                    string       `json:"rail_rpc"`                      // endpoint the chain adaptor dials; required where the world has a chain
	BootstrapPeers             *[]string    `json:"bootstrap_peers,omitempty"`     // seed multiaddrs; absent = the world's own seeds, [] = no announce/discovery, set = these instead (§13, D23)
	FedListenAddrs             []string     `json:"fed_listen_addrs"`              // multiaddrs the peer transport binds; empty = OS-assigned ports; a world's seed pins one so members find it at the same address after a restart (§13)
	CredentialsKey             string       `json:"credentials_key,omitempty"`     // base64url AES-256 key; generated on first boot
	RemoteRetryIntervalSeconds int64        `json:"remote_retry_interval_seconds"` // seconds between retry passes for pending remote calls (§13); <=0 → default
	PeerRetentionDays          int64        `json:"peer_retention_days"`           // days a peer may stay idle at zero balance before purge (§13); <=0 → disabled
	DiscoveryIntervalSeconds   int64        `json:"discovery_interval_seconds"`    // seconds between known-network discovery passes (§13); <=0 → default
}

// remoteRetryInterval is how often the running server re-drives pending remote-proxy calls so a
// peer coming back online settles parked calls without a restart (§13). A non-positive config
// value falls back to the 60s default.
func (c ServerConfig) remoteRetryInterval() time.Duration {
	if c.RemoteRetryIntervalSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.RemoteRetryIntervalSeconds) * time.Second
}

// discoveryInterval is how often the running server refreshes the known network (§13): advertise
// under the discovery rendezvous, enumerate providers, and pull gossip from bootstrap + discovered
// peers. Like remoteRetryInterval, a non-positive value falls back to the default so discovery
// works out of the box; the kill switch is an empty bootstrap_peers (no announce, no discover).
func (c ServerConfig) discoveryInterval() time.Duration {
	if c.DiscoveryIntervalSeconds <= 0 {
		return 300 * time.Second
	}
	return time.Duration(c.DiscoveryIntervalSeconds) * time.Second
}

// peerRetention is how long a peer may stay idle at zero balance before it is purged (§13).
// Unlike remoteRetryInterval, a non-positive value disables purging entirely (returns 0), so
// operators can opt out rather than get a silent default.
func (c ServerConfig) peerRetention() time.Duration {
	if c.PeerRetentionDays <= 0 {
		return 0
	}
	return time.Duration(c.PeerRetentionDays) * 24 * time.Hour
}

// DefaultServerConfig returns a ServerConfig populated with safe defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Native: NativeConfig{
			LLM:    NativeLLMConfig{URL: "http://localhost:11434", ChatModel: "gemma4:26b", EmbedModel: "nomic-embed-text", Price: 0},
			Lookup: NativeLookupConfig{DefaultLimit: 10, Price: 0},
			Web:    NativeWebConfig{Price: 0},
			TinyGo: NativePriceConfig{Price: 5},
		},
		// World has no default. A kernel joins one network for life, so which one is the operator's
		// to state (first boot asks); a default here would answer it for them, silently and once.
		ScriptTimeoutMS:   10000,
		ScriptMemoryBytes: 64 * 1024 * 1024,
		FeeBPS:            2000,
		RemoteBPS:         500,
		ImportBPS:         500,
		// The three money amounts are left unset here so they come from one place, the shipped
		// economy (kernel.DefaultEconomy), which a written-out file then shows the operator.
		TokenTTL:          "15m",
		AuthIssuer:        "",
		AuthAudience:      "",
		ListenAddr:        ":4040",
		LogLevel:          "info",
		LogFile:           "",
		LogFormat:         "text",
		AllowLocalSources: false,
		// No default meeting point here: it belongs to the world (D23), which is what decides
		// whose network a kernel is joining. An absent key takes the world's seeds.
		RemoteRetryIntervalSeconds: 60,
		PeerRetentionDays:          90,
		DiscoveryIntervalSeconds:   300,
	}
}

// LoadConfig reads the JSON config file at path onto the defaults. Missing fields keep their
// default; an absent file returns os.ErrNotExist, which is a first boot and nothing else, since
// first boot is this file's only writer. An unknown field is refused rather than ignored: a
// misspelled key reads exactly like one never written, and the setting the operator meant to
// change silently keeps its default.
func LoadConfig(path string) (ServerConfig, error) {
	cfg := DefaultServerConfig()
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// applyEnvOverrides applies the spec-documented runtime overrides (§14). Everything
// else is configured through config.json; environment variables are bootstrap and
// overrides only. JUICE_LOG_LEVEL overrides the log level; JUICE_CREDENTIALS_KEY is a
// runtime-only override of the §8 AES credentials key that never overwrites the config
// file; JUICE_ALLOW_LOCAL_SOURCES is the dev-only override of the §7 SSRF escape hatch
// (truthy "1"/"true" enables it, mirroring the allow_local_sources config key). Missing
// env vars are silently skipped.
func applyEnvOverrides(cfg *ServerConfig) {
	if v := os.Getenv("JUICE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("JUICE_CREDENTIALS_KEY"); v != "" {
		cfg.CredentialsKey = v
	}
	if v := os.Getenv("JUICE_ALLOW_LOCAL_SOURCES"); v != "" {
		cfg.AllowLocalSources = isTruthyEnv(v)
	}
}

// isTruthyEnv reads a boolean-ish env value: "1", "true", "yes", "on" (case-insensitive) are true.
func isTruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ---- Command-line overrides ----

// Every setting of config.json is also a flag of `kernel serve`, spelled as its key with `_`
// written `-` and a nested key as a path (`--native.llm.url`), and a flag typed on the command line
// wins over the file for that run. This is the arrangement bitcoin, redis and the docker daemon
// use; the alternative is a file and nothing else (IPFS, nginx). What juice had was neither: one
// port was a flag with no key and the other a key with no flag (§14).
//
// The flags are derived from the struct rather than declared one by one, so a new setting is a new
// flag and the two cannot drift apart.
var (
	serveFlags    *pflag.FlagSet // the flags `kernel serve` parsed; nil under every other command
	serveOverride ServerConfig   // what those flags parsed into
)

// bindConfigFlags registers one flag per setting on fs, parsing into `into`. Help shows the real
// defaults, because `into` starts as the shipped configuration.
func bindConfigFlags(fs *pflag.FlagSet, into *ServerConfig) {
	configFields(into, func(name string, f reflect.Value) {
		// The credentials key is the one setting with no flag: a process's command line is
		// readable by every user of the machine, and the key seals every stored credential.
		// It is read from the file, or from JUICE_CREDENTIALS_KEY for a run that must not
		// write it down.
		if name == "credentials-key" {
			return
		}
		usage := "sets " + strings.ReplaceAll(name, "-", "_") + " for this run"
		// A setting held as a pointer says three things — absent, empty, and a value — so it is
		// given a place to parse into before it is bound. Nothing is copied out of it unless the
		// flag was actually typed, so the file keeps its own three states.
		if f.Kind() == reflect.Ptr {
			f.Set(reflect.New(f.Type().Elem()))
			f = f.Elem()
		}
		switch p := f.Addr().Interface().(type) {
		case *string:
			fs.StringVar(p, name, *p, usage)
		case *int:
			fs.IntVar(p, name, *p, usage)
		case *int64:
			fs.Int64Var(p, name, *p, usage)
		case *bool:
			fs.BoolVar(p, name, *p, usage)
		case *[]string:
			fs.StringSliceVar(p, name, *p, usage)
		}
	})
}

// applyConfigFlags copies the settings named on the command line onto cfg, and only those: a flag
// nobody typed leaves the file's value, and the file's silence, alone. It is not written back —
// the file is what the kernel is, the command line what this run of it is — except on a first
// boot, which has no file yet and writes the effective configuration as the kernel's own.
func applyConfigFlags(cfg *ServerConfig) {
	if serveFlags == nil {
		return
	}
	dst := map[string]reflect.Value{}
	configFields(cfg, func(name string, f reflect.Value) { dst[name] = f })
	configFields(&serveOverride, func(name string, f reflect.Value) {
		if target, ok := dst[name]; ok && serveFlags.Changed(name) {
			target.Set(f)
		}
	})
}

// configFlagsGiven reports whether this command line carried any setting of the kernel's own. It
// counts settings and nothing else — `--json` is a word to the client, not an instruction to make
// a kernel — so a first boot can treat them as the operator's consent, exactly as a written file is.
func configFlagsGiven() bool {
	if serveFlags == nil {
		return false
	}
	given := false
	configFields(&serveOverride, func(name string, _ reflect.Value) {
		if serveFlags.Changed(name) {
			given = true
		}
	})
	return given
}

// configFields visits every leaf setting of a configuration, naming each as the flag that sets it.
// It is the single walk behind both halves above, so a flag is registered and applied under one name.
func configFields(c *ServerConfig, visit func(name string, field reflect.Value)) {
	var walk func(v reflect.Value, prefix string)
	walk = func(v reflect.Value, prefix string) {
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			key, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			if key == "" || key == "-" {
				continue
			}
			name := strings.ReplaceAll(key, "_", "-")
			if prefix != "" {
				name = prefix + "." + name
			}
			if f := v.Field(i); f.Kind() == reflect.Struct {
				walk(f, name)
			} else {
				visit(name, f)
			}
		}
	}
	walk(reflect.ValueOf(c).Elem(), "")
}

func writeConfig(path string, cfg ServerConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 0600: this file holds credentials_key, which seals every stored upstream credential.
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// KernelConfig translates the JSON (wire) configuration into the kernel's runtime Config and
// validates it — the single owner of both, so defaults and bounds cannot drift between the file
// format and the running kernel (§14). The two types stay distinct because their representations
// genuinely differ: durations are strings on the wire and time.Duration at runtime, and the token
// secret is never stored in the file at all (it is passed in).
func (c ServerConfig) KernelConfig(tokenSecret string) (kernel.Config, error) {
	cfg := kernel.DefaultConfig()
	if tokenSecret != "" {
		cfg.TokenSecret = tokenSecret
	}
	tokenTTL, err := time.ParseDuration(c.TokenTTL)
	if err != nil {
		return kernel.Config{}, fmt.Errorf("token_ttl invalid: %w", err)
	}
	cfg.TokenTTL = tokenTTL
	cfg.ScriptTimeout = time.Duration(c.ScriptTimeoutMS) * time.Millisecond
	cfg.ScriptMemory = c.ScriptMemoryBytes
	cfg.AllowLocalSources = c.AllowLocalSources
	cfg.AuthIssuer = c.AuthIssuer
	cfg.AuthAudience = c.AuthAudience
	cfg.PeerRetention = c.peerRetention()
	cfg.DiscoveryInterval = c.discoveryInterval()
	return cfg, nil
}

// Economy assembles the money rules from this configuration alone (P10): the ticket this kernel
// writes, the largest it will accept from a buyer, and how much unpaid work it will carry. All
// three are the operator's own, defaulted from the shipped economy.
func (c ServerConfig) Economy() (kernel.Economy, error) {
	econ := kernel.DefaultEconomy()
	for _, bps := range []struct {
		name  string
		value int64
		dst   *int64
	}{
		{"fee_bps", c.FeeBPS, &econ.FeeBPS},
		{"remote_bps", c.RemoteBPS, &econ.RemoteBPS},
		{"import_bps", c.ImportBPS, &econ.ImportBPS},
	} {
		if bps.value < 0 || bps.value > 10000 {
			return kernel.Economy{}, fmt.Errorf("%s must be between 0 and 10000 (basis points; 100 = 1%%)", bps.name)
		}
		*bps.dst = bps.value
	}
	for _, amount := range []struct {
		name  string
		value *int64
		dst   *int64
	}{
		{"lottery", c.Lottery, &econ.Lottery},
		{"lottery_max", c.LotteryMax, &econ.LotteryMax},
		{"credit_limit", c.CreditLimit, &econ.CreditLimit},
	} {
		if amount.value == nil {
			continue
		}
		if *amount.value < 0 {
			return kernel.Economy{}, fmt.Errorf("%s must not be negative", amount.name)
		}
		*amount.dst = *amount.value
	}
	// A kernel that would not accept its own ticket could never be paid for what it sells.
	if econ.Lottery > econ.LotteryMax {
		return kernel.Economy{}, fmt.Errorf("lottery %d is above this kernel's own lottery_max of %d", econ.Lottery, econ.LotteryMax)
	}
	return econ, nil
}

// kernelName is the nickname of the kernel this process serves, given positionally to `serve`: what
// it calls itself on the network (D15) and, being the one name chosen by the time a home is created,
// that home's directory name. The directory may be renamed without the network noticing.
var kernelName string

// validateLocalName accepts the labels this installation may turn into one file or directory name:
// a kernel under kernels/, a context under client/. The rules are the filesystem's, not the
// kernel's — a local label has no relation to a handle, so it borrows no validator from the account
// namespace — and they are one rule rather than two because both end up as a path segment, where a
// name free to hold a separator or a dot could address something other than its own.
// kind names the thing in the message, so an operator is told which label was refused.
func validateLocalName(kind, name string) error {
	if name == "" {
		return kernel.ErrInvalidInput.Wrapf("%s name is required", kind)
	}
	if len(name) > 64 {
		return kernel.ErrInvalidInput.Wrapf("%s name must be at most 64 characters", kind)
	}
	if strings.HasPrefix(name, ".") {
		return kernel.ErrInvalidInput.Wrapf("%s name must not begin with a dot", kind)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return kernel.ErrInvalidInput.Wrapf("%s name may hold only letters, digits, dot, dash and underscore", kind)
		}
	}
	return nil
}

// kernelHome is one kernel's whole home: $JUICE_HOME/kernels/<name>/, holding the database
// (and with it the signing key), config.json, the rail key and its records, the single-server lock
// and the purgeable cache. Everything that binds a kernel to its identity sits in this one
// directory, so it backs up, moves and locks as a unit, and a second kernel on the machine is a
// sibling of the first rather than a second installation.
func kernelHome() string { return filepath.Join(juiceHome(), "kernels", kernelName) }

// exists reports whether a path is there, which for a kernel's database is the whole of "has this
// kernel been created".
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// kernelsHere names the kernels of this installation: the directories under kernels/ that hold a
// database. A directory without one is a boot that was answered and then abandoned, and naming it
// as a kernel would be a lie.
func kernelsHere() []string {
	root := filepath.Join(juiceHome(), "kernels")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && exists(filepath.Join(root, e.Name(), "juice.db")) {
			names = append(names, e.Name())
		}
	}
	return names
}

// legacyKernelHome is the layout before kernels were named, where the root held exactly one. It is
// read only by migrateLegacyHome.
func legacyKernelHome() string { return filepath.Join(juiceHome(), "kernel") }

// migrateLegacyHome moves an unnamed legacy kernel into the named home and is the only writer of that
// path. Both directories live under one root, so the move is a single rename: it either happened or
// it did not, and an interrupted boot leaves no half-moved ledger. It refuses rather than merges
// when a server still holds the old home or when the destination already exists, since either case
// means two kernels are in play and only the operator can say which is wanted. Running it again
// finds nothing to move.
func migrateLegacyHome() error {
	legacy := legacyKernelHome()
	if _, err := os.Stat(filepath.Join(legacy, "juice.db")); err != nil {
		return nil // no legacy kernel here
	}
	dest := kernelHome()
	if _, err := os.Stat(dest); err == nil {
		return kernel.ErrInvalidState.Wrapf(
			"both %s and %s hold a kernel; move or remove one, since only you can say which this installation serves", legacy, dest)
	}
	// The old home's own lock is what a running server holds. Taking it proves nothing is serving
	// that ledger, so the rename cannot pull the database out from under a live process.
	lock, err := os.OpenFile(filepath.Join(legacy, "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return kernel.ErrInvalidState.Wrapf("lock %s: %v", legacy, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return kernel.ErrInvalidState.Wrapf("a server is still running for %s; stop it before this kernel moves to %s", legacy, dest)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := os.MkdirAll(filepath.Join(juiceHome(), "kernels"), 0o700); err != nil {
		return kernel.ErrInvalidState.Wrapf("create %s: %v", filepath.Dir(dest), err)
	}
	if err := os.Rename(legacy, dest); err != nil {
		return kernel.ErrInvalidState.Wrapf("move %s to %s: %v", legacy, dest, err)
	}
	fmt.Fprintf(os.Stderr, "moved kernel home %s to %s\n", legacy, dest)
	return nil
}

// bootstrapPeers is where this kernel looks for the network before it knows anyone. The world
// names its own seeds (D23), because a seed serving another network can only ever answer that it
// serves another network. The config key overrides them in three states an operator can tell
// apart: absent takes the world's, an empty list means no meeting point at all (announce and
// discover nothing), and a list of addresses replaces them.
func (c ServerConfig) bootstrapPeers(w rail.World) []string {
	if c.BootstrapPeers != nil {
		return *c.BootstrapPeers
	}
	return w.Seeds
}
