package main

import (
	"encoding/json"
	"fmt"
	"github.com/daios-ai/juice/kernel"
	"os"
	"strings"
	"time"
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
	HTTPCallbackURL            string       `json:"http_callback_url"`             // base URL advertised to dispatched kind=http endpoints for capability callbacks (§9); "" ⇒ derive from listen address
	KernelHandle               string       `json:"kernel_handle"`                 // handle this kernel presents in gossip (§13)
	World                      string       `json:"world"`                         // the network this kernel serves: play, test, real, or a world file's path (D23)
	RailRPC                    string       `json:"rail_rpc"`                      // endpoint the chain adaptor dials; required where the world has a chain
	BootstrapPeers             []string     `json:"bootstrap_peers"`               // seed multiaddrs; sole seed source; empty = no announce/discovery (§13)
	FedListenAddrs             []string     `json:"fed_listen_addrs"`              // multiaddrs the peer transport binds; empty = OS-assigned ports; a public node pins one so peers find it at the same address after a restart (§13)
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
		LogLevel:          "info",
		LogFile:           "",
		LogFormat:         "text",
		AllowLocalSources: false,
		// The public daios.ai node is the default meeting point, so a fresh `juice serve` joins
		// the network out of the box (it listens on the standard port 31313, §13). Override or
		// extend for a private network; clear it to run standalone.
		BootstrapPeers:             []string{"/dns4/daios.ai/tcp/31313/p2p/12D3KooWJ5ZwPSAV17q2hv6ttZ8J3hHsTMNaSC61kbVbvxvtArjK"},
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
