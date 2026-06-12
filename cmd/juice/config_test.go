package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultServerConfig(t *testing.T) {
	cfg := DefaultServerConfig()
	if cfg.OllamaURL == "" {
		t.Error("OllamaURL should have a default")
	}
	if cfg.ScriptTimeoutMS <= 0 {
		t.Error("ScriptTimeoutMS should be positive")
	}
	if cfg.ScriptMemoryBytes <= 0 {
		t.Error("ScriptMemoryBytes should be positive")
	}
	if cfg.TokenTTL == "" {
		t.Error("TokenTTL should have a default")
	}
	if cfg.MakeMaxSteps <= 0 {
		t.Error("MakeMaxSteps should be positive")
	}
}

func TestLoadOrCreateConfig_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "juice.json")

	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OllamaURL != DefaultServerConfig().OllamaURL {
		t.Errorf("expected default OllamaURL, got %q", cfg.OllamaURL)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created: %v", err)
	}
}

func TestLoadOrCreateConfig_ReadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "juice.json")

	want := DefaultServerConfig()
	want.OllamaURL = "http://custom:11434"
	want.MakeMaxSteps = 3
	b, _ := json.MarshalIndent(want, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OllamaURL != "http://custom:11434" {
		t.Errorf("expected custom OllamaURL, got %q", cfg.OllamaURL)
	}
	if cfg.MakeMaxSteps != 3 {
		t.Errorf("expected MakeMaxSteps=3, got %d", cfg.MakeMaxSteps)
	}
}

func TestLoadOrCreateConfig_MissingFieldsUseDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "juice.json")

	// Write partial config — only one field
	if err := os.WriteFile(path, []byte(`{"ollama_url":"http://custom:11434"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OllamaURL != "http://custom:11434" {
		t.Errorf("expected custom OllamaURL, got %q", cfg.OllamaURL)
	}
	if cfg.MakeMaxSteps != DefaultServerConfig().MakeMaxSteps {
		t.Errorf("expected default MakeMaxSteps, got %d", cfg.MakeMaxSteps)
	}
}

func TestLoadOrCreateConfig_BadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "juice.json")
	if err := os.WriteFile(path, []byte(`{bad json`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateConfig(path)
	if err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Run("overrides file values", func(t *testing.T) {
		t.Setenv("JUICE_LOG_LEVEL", "debug")
		t.Setenv("JUICE_FEE_BPS", "500")
		t.Setenv("JUICE_OLLAMA_URL", "http://custom:11434")
		t.Setenv("JUICE_SCRIPT_TIMEOUT_MS", "5000")
		t.Setenv("JUICE_SCRIPT_MEMORY_BYTES", "33554432")
		t.Setenv("JUICE_TOKEN_TTL", "30m")
		t.Setenv("JUICE_AUTH_ISSUER", "https://issuer.example")
		t.Setenv("JUICE_AUTH_AUDIENCE", "juice")
		t.Setenv("JUICE_OLLAMA_CHAT_MODEL", "llama3")
		t.Setenv("JUICE_OLLAMA_EMBED_MODEL", "all-minilm")
		t.Setenv("JUICE_LOG_FILE", "/tmp/juice.log")

		cfg := DefaultServerConfig()
		if err := applyEnvOverrides(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.LogLevel != "debug" {
			t.Errorf("LogLevel: got %q", cfg.LogLevel)
		}
		if cfg.FeeBPS != 500 {
			t.Errorf("FeeBPS: got %d", cfg.FeeBPS)
		}
		if cfg.OllamaURL != "http://custom:11434" {
			t.Errorf("OllamaURL: got %q", cfg.OllamaURL)
		}
		if cfg.ScriptTimeoutMS != 5000 {
			t.Errorf("ScriptTimeoutMS: got %d", cfg.ScriptTimeoutMS)
		}
		if cfg.ScriptMemoryBytes != 33554432 {
			t.Errorf("ScriptMemoryBytes: got %d", cfg.ScriptMemoryBytes)
		}
		if cfg.TokenTTL != "30m" {
			t.Errorf("TokenTTL: got %q", cfg.TokenTTL)
		}
		if cfg.AuthIssuer != "https://issuer.example" {
			t.Errorf("AuthIssuer: got %q", cfg.AuthIssuer)
		}
		if cfg.AuthAudience != "juice" {
			t.Errorf("AuthAudience: got %q", cfg.AuthAudience)
		}
		if cfg.OllamaChatModel != "llama3" {
			t.Errorf("OllamaChatModel: got %q", cfg.OllamaChatModel)
		}
		if cfg.OllamaEmbedModel != "all-minilm" {
			t.Errorf("OllamaEmbedModel: got %q", cfg.OllamaEmbedModel)
		}
		if cfg.LogFile != "/tmp/juice.log" {
			t.Errorf("LogFile: got %q", cfg.LogFile)
		}
	})

	t.Run("absent env leaves file value", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.OllamaURL = "http://from-file:11434"
		cfg.FeeBPS = 1234
		if err := applyEnvOverrides(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.OllamaURL != "http://from-file:11434" {
			t.Errorf("OllamaURL should not be overridden, got %q", cfg.OllamaURL)
		}
		if cfg.FeeBPS != 1234 {
			t.Errorf("FeeBPS should not be overridden, got %d", cfg.FeeBPS)
		}
	})

	t.Run("bad JUICE_FEE_BPS returns error", func(t *testing.T) {
		t.Setenv("JUICE_FEE_BPS", "notanumber")
		cfg := DefaultServerConfig()
		if err := applyEnvOverrides(&cfg); err == nil {
			t.Error("expected error for bad JUICE_FEE_BPS")
		}
	})

	t.Run("bad JUICE_SCRIPT_TIMEOUT_MS returns error", func(t *testing.T) {
		t.Setenv("JUICE_SCRIPT_TIMEOUT_MS", "bad")
		cfg := DefaultServerConfig()
		if err := applyEnvOverrides(&cfg); err == nil {
			t.Error("expected error for bad JUICE_SCRIPT_TIMEOUT_MS")
		}
	})

	t.Run("bad JUICE_SCRIPT_MEMORY_BYTES returns error", func(t *testing.T) {
		t.Setenv("JUICE_SCRIPT_MEMORY_BYTES", "bad")
		cfg := DefaultServerConfig()
		if err := applyEnvOverrides(&cfg); err == nil {
			t.Error("expected error for bad JUICE_SCRIPT_MEMORY_BYTES")
		}
	})

	t.Run("JUICE_CREDENTIALS_KEY overrides config", func(t *testing.T) {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i + 1)
		}
		t.Setenv("JUICE_CREDENTIALS_KEY", base64.RawURLEncoding.EncodeToString(key))
		cfg := DefaultServerConfig()
		if err := applyEnvOverrides(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.CredentialsKey != base64.RawURLEncoding.EncodeToString(key) {
			t.Errorf("CredentialsKey not overridden, got %q", cfg.CredentialsKey)
		}
	})
}

func TestAESGCMBox(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	box, err := newAESGCMBox(key)
	if err != nil {
		t.Fatalf("newAESGCMBox: %v", err)
	}

	plaintext := `{"scheme":"bearer","secrets":{"token":"secret-token"}}`
	aad := "action-id-123"

	ct, err := box.Seal(aad, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Ciphertext must not contain the plaintext token.
	raw, _ := base64.RawURLEncoding.DecodeString(ct)
	if strings.Contains(string(raw), "secret-token") {
		t.Error("ciphertext must not contain plaintext secret")
	}

	// Round-trip succeeds.
	recovered, err := box.Open(aad, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if recovered != plaintext {
		t.Errorf("round-trip mismatch: got %q, want %q", recovered, plaintext)
	}

	// Wrong AAD fails.
	if _, err := box.Open("wrong-action-id", ct); err == nil {
		t.Error("expected error with wrong AAD")
	}

	// Tampered ciphertext fails.
	tampered := make([]byte, len(raw))
	copy(tampered, raw)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := box.Open(aad, base64.RawURLEncoding.EncodeToString(tampered)); err == nil {
		t.Error("expected error with tampered ciphertext")
	}

	// Wrong key size rejected.
	if _, err := newAESGCMBox([]byte("tooshort")); err == nil {
		t.Error("expected error for wrong key size")
	}
}
