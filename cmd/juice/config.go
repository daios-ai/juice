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

// NativeWebConfig holds configuration for the @sys/web native action.
type NativeWebConfig struct {
	Price     int64  `json:"price"`
	UserAgent string `json:"user_agent"`
}

// NativePriceConfig is the whole configuration of a native whose only setting is its price —
// most of the stdlib. One type instead of one struct per action; the JSON shape is unchanged (§14).
type NativePriceConfig struct {
	Price int64 `json:"price"`
}

// NativeConfig holds per-action configuration for all native actions (§14 `native.<action>`).
// Configuration owns prices and their defaults; each native's contract lives with its handler (§9).
type NativeConfig struct {
	LLM        NativeLLMConfig    `json:"llm"`
	Lookup     NativeLookupConfig `json:"lookup"`
	UserLookup NativePriceConfig  `json:"user-lookup"`
	Time       NativePriceConfig  `json:"time"`
	Sink       NativePriceConfig  `json:"sink"`
	Message    NativePriceConfig  `json:"message"`
	Random     NativePriceConfig  `json:"random"`
	Web        NativeWebConfig    `json:"web"`
	TinyGo     NativePriceConfig  `json:"tinygo"`
	Transfer   NativePriceConfig  `json:"transfer"`
}

// PriceOf returns the configured price for a native action name (§9 names, §14 config keys). It is
// the single place the two vocabularies meet, so bootstrap never restates either.
func (c NativeConfig) PriceOf(name string) int64 {
	switch name {
	case "lookup":
		return c.Lookup.Price
	case "user-lookup":
		return c.UserLookup.Price
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
	RemoteBPS                  int64        `json:"remote_bps"`         // serving-side markup on inbound remote calls (§13)
	ImportBPS                  int64        `json:"import_bps"`         // origin-side import fee on outbound remote calls, retained locally (§13)
	ExposureMax                int64        `json:"exposure_max"`       // X: max gross unsecured receivables across all peers (§13); 0 = prepaid-only
	SettlementTrigger          int64        `json:"settlement_trigger"` // Y: gross-receivables level flagging settlement_due (§13); 0 < Y < X when X > 0
	SettlementQuantum          int64        `json:"settlement_quantum"` // Q: smallest fee-rational external payment (§13); 0 disables the probabilistic path
	TokenTTL                   string       `json:"token_ttl"`
	AuthIssuer                 string       `json:"auth_issuer"`
	AuthAudience               string       `json:"auth_audience"`
	LogLevel                   string       `json:"log_level"`
	LogFile                    string       `json:"log_file"`
	LogFormat                  string       `json:"log_format"`
	AllowLocalSources          bool         `json:"allow_local_sources"`
	ServerURL                  string       `json:"server_url"`                    // local base URL the CLI dials; never a federation identity (§14)
	HTTPCallbackURL            string       `json:"http_callback_url"`             // base URL advertised to dispatched kind=http endpoints for capability callbacks (§9); "" ⇒ derive from listen address
	KernelHandle               string       `json:"kernel_handle"`                 // handle this kernel presents in gossip (§13)
	BootstrapPeers             []string     `json:"bootstrap_peers"`               // seed multiaddrs; sole seed source; empty = no announce/discovery (§13)
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
			Web:    NativeWebConfig{Price: 0, UserAgent: "juice-kernel/0.4 (+https://github.com/daios-ai/juice)"},
			TinyGo: NativePriceConfig{Price: 5},
		},
		ScriptTimeoutMS:   10000,
		ScriptMemoryBytes: 64 * 1024 * 1024,
		FeeBPS:            2000,
		RemoteBPS:         500,
		ImportBPS:         500,
		// A fresh kernel serves remote paid calls out of the box (§13): X=1000 caps the total
		// unsecured credit it extends across all peers (a bounded, Sybil-proof maximum loss),
		// flagged for settlement at Y=500. Set exposure_max=0 to opt into prepaid-only. Q stays 0
		// (rail-dependent; the operator sets it from F/r to enable the probabilistic residual path).
		ExposureMax:       1000,
		SettlementTrigger: 500,
		SettlementQuantum: 0,
		TokenTTL:          "15m",
		AuthIssuer:        "",
		AuthAudience:      "",
		LogLevel:          "info",
		LogFile:           "",
		LogFormat:         "text",
		AllowLocalSources: false,
		ServerURL:         "",
		// The public daios.ai node is the default meeting point, so a fresh `juice serve` joins
		// the network out of the box (it listens on the standard port 31313, §13). Override or
		// extend for a private network; clear it to run standalone.
		BootstrapPeers:             []string{"/dns4/daios.ai/tcp/31313/p2p/12D3KooWJ5ZwPSAV17q2hv6ttZ8J3hHsTMNaSC61kbVbvxvtArjK"},
		RemoteRetryIntervalSeconds: 60,
		PeerRetentionDays:          90,
		DiscoveryIntervalSeconds:   300,
	}
}

// LoadOrCreateConfig reads the JSON config file at path.
// If the file does not exist it is created with defaults and the defaults are returned.
// Missing fields in an existing file are filled with defaults.
func LoadOrCreateConfig(path string) (ServerConfig, error) {
	cfg := DefaultServerConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if writeErr := writeConfig(path, cfg); writeErr != nil {
				return cfg, fmt.Errorf("create default config %q: %w", path, writeErr)
			}
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
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
	return os.WriteFile(path, append(b, '\n'), 0o644)
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

	for _, bps := range []struct {
		name  string
		value int64
		dst   *int64
	}{
		{"fee_bps", c.FeeBPS, &cfg.FeeBPS},
		{"remote_bps", c.RemoteBPS, &cfg.RemoteBPS},
		{"import_bps", c.ImportBPS, &cfg.ImportBPS},
	} {
		if bps.value < 0 || bps.value > 10000 {
			return kernel.Config{}, fmt.Errorf("%s must be 0–10000", bps.name)
		}
		*bps.dst = bps.value
	}

	// Global exposure policy (§13): X ≥ 0; when X > 0 the settlement trigger must sit strictly inside
	// it (0 < Y < X) so a flagged peer is still below the hard cap; Q ≥ 0 (0 disables the residual path).
	if c.ExposureMax < 0 || c.SettlementQuantum < 0 {
		return kernel.Config{}, fmt.Errorf("exposure_max and settlement_quantum must be non-negative")
	}
	if c.ExposureMax > 0 && !(c.SettlementTrigger > 0 && c.SettlementTrigger < c.ExposureMax) {
		return kernel.Config{}, fmt.Errorf("settlement_trigger must satisfy 0 < settlement_trigger < exposure_max when exposure_max > 0")
	}
	cfg.ExposureMax = c.ExposureMax
	cfg.SettlementTrigger = c.SettlementTrigger
	cfg.SettlementQuantum = c.SettlementQuantum

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
