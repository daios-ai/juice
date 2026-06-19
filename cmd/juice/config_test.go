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
	if cfg.Native.LLM.URL == "" {
		t.Error("Native.LLM.URL should have a default")
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
	if cfg.Native.Make.MaxSteps <= 0 {
		t.Error("Native.Make.MaxSteps should be positive")
	}
	if cfg.Native.Make.Price != 20 {
		t.Errorf("Native.Make.Price default = %d, want 20", cfg.Native.Make.Price)
	}
	if cfg.Native.TinyGo.Price != 5 {
		t.Errorf("Native.TinyGo.Price default = %d, want 5", cfg.Native.TinyGo.Price)
	}
}

func TestLoadOrCreateConfig_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "juice.json")

	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Native.LLM.URL != DefaultServerConfig().Native.LLM.URL {
		t.Errorf("expected default Native.LLM.URL, got %q", cfg.Native.LLM.URL)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created: %v", err)
	}
}

func TestLoadOrCreateConfig_ReadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "juice.json")

	want := DefaultServerConfig()
	want.Native.LLM.URL = "http://custom:11434"
	want.Native.Make.MaxSteps = 3
	b, _ := json.MarshalIndent(want, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Native.LLM.URL != "http://custom:11434" {
		t.Errorf("expected custom Native.LLM.URL, got %q", cfg.Native.LLM.URL)
	}
	if cfg.Native.Make.MaxSteps != 3 {
		t.Errorf("expected Native.Make.MaxSteps=3, got %d", cfg.Native.Make.MaxSteps)
	}
}

func TestLoadOrCreateConfig_MissingFieldsUseDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "juice.json")

	// Write partial config — only one nested field
	if err := os.WriteFile(path, []byte(`{"native":{"llm":{"url":"http://custom:11434"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadOrCreateConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Native.LLM.URL != "http://custom:11434" {
		t.Errorf("expected custom Native.LLM.URL, got %q", cfg.Native.LLM.URL)
	}
	if cfg.Native.Make.MaxSteps != DefaultServerConfig().Native.Make.MaxSteps {
		t.Errorf("expected default Native.Make.MaxSteps, got %d", cfg.Native.Make.MaxSteps)
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
	// Only the spec-documented runtime overrides remain (§14): JUICE_LOG_LEVEL and the
	// runtime-only JUICE_CREDENTIALS_KEY. Everything else is configured via juice.json.
	t.Run("overrides log level", func(t *testing.T) {
		t.Setenv("JUICE_LOG_LEVEL", "debug")
		cfg := DefaultServerConfig()
		applyEnvOverrides(&cfg)
		if cfg.LogLevel != "debug" {
			t.Errorf("LogLevel: got %q", cfg.LogLevel)
		}
	})

	t.Run("absent env leaves file value", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.LogLevel = "warn"
		cfg.Native.LLM.URL = "http://from-file:11434"
		cfg.FeeBPS = 1234
		applyEnvOverrides(&cfg)
		if cfg.LogLevel != "warn" {
			t.Errorf("LogLevel should not be overridden, got %q", cfg.LogLevel)
		}
		if cfg.Native.LLM.URL != "http://from-file:11434" {
			t.Errorf("Native.LLM.URL should not be overridden, got %q", cfg.Native.LLM.URL)
		}
		if cfg.FeeBPS != 1234 {
			t.Errorf("FeeBPS should not be overridden, got %d", cfg.FeeBPS)
		}
	})

	t.Run("JUICE_CREDENTIALS_KEY overrides config", func(t *testing.T) {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i + 1)
		}
		t.Setenv("JUICE_CREDENTIALS_KEY", base64.RawURLEncoding.EncodeToString(key))
		cfg := DefaultServerConfig()
		applyEnvOverrides(&cfg)
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
