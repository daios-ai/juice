package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
