package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// NativeLLMConfig holds configuration for the @sys/llm/chat native action.
type NativeLLMConfig struct {
	URL        string `json:"url"`
	ChatModel  string `json:"chat_model"`
	EmbedModel string `json:"embed_model"`
	Price      int64  `json:"price"`
}

// NativeMakeConfig holds configuration for the @sys/make native action.
type NativeMakeConfig struct {
	Compiler string `json:"compiler"`
	MaxSteps int    `json:"max_steps"`
	Price    int64  `json:"price"`
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

// NativeConfig holds per-action configuration for all native actions.
type NativeConfig struct {
	LLM     NativeLLMConfig     `json:"llm"`
	Make    NativeMakeConfig    `json:"make"`
	Lookup  NativeLookupConfig  `json:"lookup"`
	Time    NativeTimeConfig    `json:"time"`
	Sink    NativeSinkConfig    `json:"sink"`
	Message NativeMessageConfig `json:"message"`
	Random  NativeRandomConfig  `json:"random"`
}

// ServerConfig holds all non-secret runtime configuration.
// Secrets (JUICE_SECRET_KEY, JUICE_BOOTSTRAP_PASSWORD) are read from environment variables.
// All other settings come from this struct, populated from the JSON config file.
type ServerConfig struct {
	Native            NativeConfig `json:"native"`
	ScriptTimeoutMS   int64        `json:"script_timeout_ms"`
	ScriptMemoryBytes int64        `json:"script_memory_bytes"`
	FeeBPS            int64        `json:"fee_bps"`
	ImportBPS         int64        `json:"import_bps"`
	TokenTTL          string       `json:"token_ttl"`
	AuthIssuer        string       `json:"auth_issuer"`
	AuthAudience      string       `json:"auth_audience"`
	LogLevel          string       `json:"log_level"`
	LogFile           string       `json:"log_file"`
	LogFormat         string       `json:"log_format"`
	AllowLocalSources  bool         `json:"allow_local_sources"`
	AllowLocalPeerURLs bool         `json:"allow_local_peer_urls"`
	ServerURL         string       `json:"server_url"`
	PeerAutoAccept    bool         `json:"peer_auto_accept"`
	PeerHandle        string       `json:"peer_handle"`
	CredentialsKey    string       `json:"credentials_key,omitempty"` // base64url AES-256 key; generated on first boot
}

// DefaultServerConfig returns a ServerConfig populated with safe defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Native: NativeConfig{
			LLM:     NativeLLMConfig{URL: "http://localhost:11434", ChatModel: "gemma4:26b", EmbedModel: "nomic-embed-text", Price: 0},
			Make:    NativeMakeConfig{Compiler: "tinygo", MaxSteps: 5, Price: 20},
			Lookup:  NativeLookupConfig{DefaultLimit: 10, Price: 0},
			Time:    NativeTimeConfig{Price: 0},
			Sink:    NativeSinkConfig{Price: 0},
			Message: NativeMessageConfig{Price: 0},
			Random:  NativeRandomConfig{Price: 0},
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
		AllowLocalSources:  false,
		AllowLocalPeerURLs: false,
		ServerURL:         "",
		PeerAutoAccept:    true,
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

// applyEnvOverrides reads JUICE_* environment variables and overrides the matching
// config fields. An unparseable numeric or duration value is a fatal startup error.
// Returns a non-nil error only on parse failures; missing env vars are silently skipped.
func applyEnvOverrides(cfg *ServerConfig) error {
	if v := os.Getenv("JUICE_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("JUICE_LOG_FILE"); v != "" {
		cfg.LogFile = v
	}
	if v := os.Getenv("JUICE_FEE_BPS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("JUICE_FEE_BPS: %w", err)
		}
		cfg.FeeBPS = n
	}
	if v := os.Getenv("JUICE_AUTH_ISSUER"); v != "" {
		cfg.AuthIssuer = v
	}
	if v := os.Getenv("JUICE_AUTH_AUDIENCE"); v != "" {
		cfg.AuthAudience = v
	}
	if v := os.Getenv("JUICE_TOKEN_TTL"); v != "" {
		cfg.TokenTTL = v
	}
	if v := os.Getenv("JUICE_OLLAMA_URL"); v != "" {
		cfg.Native.LLM.URL = v
	}
	if v := os.Getenv("JUICE_OLLAMA_CHAT_MODEL"); v != "" {
		cfg.Native.LLM.ChatModel = v
	}
	if v := os.Getenv("JUICE_OLLAMA_EMBED_MODEL"); v != "" {
		cfg.Native.LLM.EmbedModel = v
	}
	if v := os.Getenv("JUICE_SCRIPT_TIMEOUT_MS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("JUICE_SCRIPT_TIMEOUT_MS: %w", err)
		}
		cfg.ScriptTimeoutMS = n
	}
	if v := os.Getenv("JUICE_SCRIPT_MEMORY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("JUICE_SCRIPT_MEMORY_BYTES: %w", err)
		}
		cfg.ScriptMemoryBytes = n
	}
	// JUICE_CREDENTIALS_KEY is a runtime-only override; it never overwrites the config file.
	if v := os.Getenv("JUICE_CREDENTIALS_KEY"); v != "" {
		cfg.CredentialsKey = v
	}
	return nil
}

func writeConfig(path string, cfg ServerConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
