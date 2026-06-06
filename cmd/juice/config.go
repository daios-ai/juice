package main

import (
	"encoding/json"
	"fmt"
	"os"
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

func writeConfig(path string, cfg ServerConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
