package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// ServerConfig holds all non-secret runtime configuration.
// Secrets (JUICE_SECRET_KEY, JUICE_BOOTSTRAP_PASSWORD) are read from environment variables.
// All other settings come from this struct, populated from the JSON config file.
type ServerConfig struct {
	OllamaURL         string `json:"ollama_url"`
	OllamaChatModel   string `json:"ollama_chat_model"`
	OllamaEmbedModel  string `json:"ollama_embed_model"`
	ScriptTimeoutMS   int64  `json:"script_timeout_ms"`
	ScriptMemoryBytes int64  `json:"script_memory_bytes"`
	FeeBPS            int64  `json:"fee_bps"`
	TokenTTL          string `json:"token_ttl"`
	AuthIssuer        string `json:"auth_issuer"`
	AuthAudience      string `json:"auth_audience"`
	LogLevel          string `json:"log_level"`
	LogFile           string `json:"log_file"`
	LogFormat         string `json:"log_format"`
	MakeMaxSteps      int    `json:"make_max_steps"`
	AllowLocalSources bool   `json:"allow_local_sources"`
	ServerURL         string `json:"server_url"`
	SMTPHost          string `json:"smtp_host"`
	SMTPPort          int    `json:"smtp_port"`
	SMTPUser          string `json:"smtp_user"`
	SMTPPassword      string `json:"smtp_password"`
	SMTPFrom          string `json:"smtp_from"`
}

// DefaultServerConfig returns a ServerConfig populated with safe defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		OllamaURL:         "http://localhost:11434",
		OllamaChatModel:   "gemma4:26b",
		OllamaEmbedModel:  "nomic-embed-text",
		ScriptTimeoutMS:   10000,
		ScriptMemoryBytes: 64 * 1024 * 1024,
		FeeBPS:            2000,
		TokenTTL:          "15m",
		AuthIssuer:        "",
		AuthAudience:      "",
		LogLevel:          "info",
		LogFile:           "",
		LogFormat:         "text",
		MakeMaxSteps:      5,
		AllowLocalSources: false,
		ServerURL:         "",
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
		cfg.OllamaURL = v
	}
	if v := os.Getenv("JUICE_OLLAMA_CHAT_MODEL"); v != "" {
		cfg.OllamaChatModel = v
	}
	if v := os.Getenv("JUICE_OLLAMA_EMBED_MODEL"); v != "" {
		cfg.OllamaEmbedModel = v
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
	return nil
}

func writeConfig(path string, cfg ServerConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
