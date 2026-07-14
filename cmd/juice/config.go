package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// NativeLLMConfig holds configuration for the @sys/llm/chat native action.
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

// NativeTimeConfig holds configuration for the @sys/time native action.
type NativeTimeConfig struct {
	Price int64 `json:"price"`
}

// NativeSinkConfig holds configuration for the @sys/sink native action.
type NativeSinkConfig struct {
	Price int64 `json:"price"`
}

// NativeMessageConfig holds configuration for the @sys/message native action.
type NativeMessageConfig struct {
	Price int64 `json:"price"`
}

// NativeRandomConfig holds configuration for the @sys/random native action.
type NativeRandomConfig struct {
	Price int64 `json:"price"`
}

// NativeTinyGoConfig holds configuration for the @sys/tinygo/compile native action.
type NativeTinyGoConfig struct {
	Price int64 `json:"price"`
}

// NativeWebConfig holds configuration for the @sys/web native action.
type NativeWebConfig struct {
	Price     int64  `json:"price"`
	UserAgent string `json:"user_agent"`
}

// NativeConfig holds per-action configuration for all native actions.
type NativeConfig struct {
	LLM     NativeLLMConfig     `json:"llm"`
	Lookup  NativeLookupConfig  `json:"lookup"`
	Time    NativeTimeConfig    `json:"time"`
	Sink    NativeSinkConfig    `json:"sink"`
	Message NativeMessageConfig `json:"message"`
	Random  NativeRandomConfig  `json:"random"`
	Web     NativeWebConfig     `json:"web"`
	TinyGo  NativeTinyGoConfig  `json:"tinygo"`
}

// ServerConfig holds all non-secret runtime configuration.
// Secrets (JUICE_SECRET_KEY, JUICE_BOOTSTRAP_PASSWORD) are read from environment variables.
// All other settings come from this struct, populated from the JSON config file.
type ServerConfig struct {
	Native                     NativeConfig `json:"native"`
	ScriptTimeoutMS            int64        `json:"script_timeout_ms"`
	ScriptMemoryBytes          int64        `json:"script_memory_bytes"`
	FeeBPS                     int64        `json:"fee_bps"`
	ImportBPS                  int64        `json:"import_bps"`
	TokenTTL                   string       `json:"token_ttl"`
	AuthIssuer                 string       `json:"auth_issuer"`
	AuthAudience               string       `json:"auth_audience"`
	LogLevel                   string       `json:"log_level"`
	LogFile                    string       `json:"log_file"`
	LogFormat                  string       `json:"log_format"`
	AllowLocalSources          bool         `json:"allow_local_sources"`
	ServerURL                  string       `json:"server_url"` // local base URL the CLI dials; never a federation identity (§14)
	PeerAutoAccept             bool         `json:"peer_auto_accept"`
	KernelHandle               string       `json:"kernel_handle"`                 // handle this kernel presents in friend handshakes and gossip (§13)
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
			LLM:     NativeLLMConfig{URL: "http://localhost:11434", ChatModel: "gemma4:26b", EmbedModel: "nomic-embed-text", Price: 0},
			Lookup:  NativeLookupConfig{DefaultLimit: 10, Price: 0},
			Time:    NativeTimeConfig{Price: 0},
			Sink:    NativeSinkConfig{Price: 0},
			Message: NativeMessageConfig{Price: 0},
			Random:  NativeRandomConfig{Price: 0},
			Web:     NativeWebConfig{Price: 0, UserAgent: "juice-kernel/0.4 (+https://github.com/daios-ai/juice)"},
			TinyGo:  NativeTinyGoConfig{Price: 5},
		},
		ScriptTimeoutMS:   10000,
		ScriptMemoryBytes: 64 * 1024 * 1024,
		FeeBPS:            2000,
		ImportBPS:         500,
		TokenTTL:          "15m",
		AuthIssuer:        "",
		AuthAudience:      "",
		LogLevel:          "info",
		LogFile:           "",
		LogFormat:         "text",
		AllowLocalSources: false,
		ServerURL:         "",
		PeerAutoAccept:    true,
		// The public daios.ai node is the default meeting point, so a fresh `juice serve` joins
		// the network out of the box (it listens on the standard port 31313, §13). Override or
		// extend for a private network; clear it to run standalone.
		BootstrapPeers:             []string{"/dns4/daios.ai/tcp/31313/p2p/12D3KooWJ5ZwPSAV17q2hv6ttZ8J3hHsTMNaSC61kbVbvxvtArjK"},
		RemoteRetryIntervalSeconds: 60,
		PeerRetentionDays:          30,
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
// file. Missing env vars are silently skipped.
func applyEnvOverrides(cfg *ServerConfig) {
	if v := os.Getenv("JUICE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("JUICE_CREDENTIALS_KEY"); v != "" {
		cfg.CredentialsKey = v
	}
}

func writeConfig(path string, cfg ServerConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
