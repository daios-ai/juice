package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteRetryInterval(t *testing.T) {
	// Configured positive value is honored.
	if got := (ServerConfig{RemoteRetryIntervalSeconds: 5}).remoteRetryInterval(); got != 5*time.Second {
		t.Errorf("configured: got %v, want 5s", got)
	}
	// Zero and negative fall back to the 60s default.
	if got := (ServerConfig{RemoteRetryIntervalSeconds: 0}).remoteRetryInterval(); got != 60*time.Second {
		t.Errorf("zero: got %v, want 60s", got)
	}
	if got := (ServerConfig{RemoteRetryIntervalSeconds: -3}).remoteRetryInterval(); got != 60*time.Second {
		t.Errorf("negative: got %v, want 60s", got)
	}
	// The default config ships a sane interval.
	if DefaultServerConfig().remoteRetryInterval() != 60*time.Second {
		t.Errorf("default config interval = %v, want 60s", DefaultServerConfig().remoteRetryInterval())
	}
}

func TestPeerRetention(t *testing.T) {
	// Configured positive value converts days → duration.
	if got := (ServerConfig{PeerRetentionDays: 30}).peerRetention(); got != 30*24*time.Hour {
		t.Errorf("configured: got %v, want 720h", got)
	}
	// Zero and negative disable purging (0 duration), NOT a silent default.
	if got := (ServerConfig{PeerRetentionDays: 0}).peerRetention(); got != 0 {
		t.Errorf("zero: got %v, want 0 (disabled)", got)
	}
	if got := (ServerConfig{PeerRetentionDays: -5}).peerRetention(); got != 0 {
		t.Errorf("negative: got %v, want 0 (disabled)", got)
	}
	// The default config ships a 90-day retention (§14).
	if DefaultServerConfig().peerRetention() != 90*24*time.Hour {
		t.Errorf("default config retention = %v, want 2160h", DefaultServerConfig().peerRetention())
	}
}

func TestDiscoveryInterval(t *testing.T) {
	// Configured positive value converts seconds → duration.
	if got := (ServerConfig{DiscoveryIntervalSeconds: 42}).discoveryInterval(); got != 42*time.Second {
		t.Errorf("configured: got %v, want 42s", got)
	}
	// Zero and negative fall back to the default (discovery works out of the box).
	if got := (ServerConfig{DiscoveryIntervalSeconds: 0}).discoveryInterval(); got != 300*time.Second {
		t.Errorf("zero: got %v, want 300s", got)
	}
	if got := (ServerConfig{DiscoveryIntervalSeconds: -1}).discoveryInterval(); got != 300*time.Second {
		t.Errorf("negative: got %v, want 300s", got)
	}
	if DefaultServerConfig().discoveryInterval() != 300*time.Second {
		t.Errorf("default config discovery = %v, want 300s", DefaultServerConfig().discoveryInterval())
	}
}

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
	if cfg.Native.TinyGo.Price != 5 {
		t.Errorf("Native.TinyGo.Price default = %d, want 5", cfg.Native.TinyGo.Price)
	}
}

// LoadConfig never creates: first boot is the only writer of config.json, so an absent file is a
// first boot and nothing else.
func TestLoadConfig_AbsentIsNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := LoadConfig(path); !os.IsNotExist(err) {
		t.Fatalf("absent config: got %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("reading a config created one")
	}
}

func TestLoadConfig_ReadsExistingOntoDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"native":{"llm":{"url":"http://custom:11434"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Native.LLM.URL != "http://custom:11434" {
		t.Errorf("Native.LLM.URL = %q", cfg.Native.LLM.URL)
	}
	if cfg.Native.LLM.ChatModel != DefaultServerConfig().Native.LLM.ChatModel {
		t.Errorf("an unstated field must keep its default, got %q", cfg.Native.LLM.ChatModel)
	}
}

// A key the operator misspelled reads exactly like a key they never wrote, and the setting they
// meant to change silently keeps its default — which for `world` is a decision they cannot revisit.
func TestLoadConfig_UnknownKeyIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"wolrd":"real"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "wolrd") {
		t.Errorf("the refusal must name the key: %v", err)
	}
}

// There is no default world: a kernel joins one network for life, so which one is the operator's
// to state and first boot asks for it.
func TestDefaultConfigNamesNoWorld(t *testing.T) {
	if w := DefaultServerConfig().World; w != "" {
		t.Errorf("DefaultServerConfig().World = %q, want empty", w)
	}
}

func TestLoadConfig_BadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{bad json`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	// Only the spec-documented runtime overrides remain (§14): JUICE_LOG_LEVEL and the
	// runtime-only JUICE_CREDENTIALS_KEY. Everything else is configured via config.json.
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

	t.Run("JUICE_ALLOW_LOCAL_SOURCES truthy enables the escape hatch", func(t *testing.T) {
		t.Setenv("JUICE_ALLOW_LOCAL_SOURCES", "1")
		cfg := DefaultServerConfig()
		applyEnvOverrides(&cfg)
		if !cfg.AllowLocalSources {
			t.Error("AllowLocalSources should be enabled by JUICE_ALLOW_LOCAL_SOURCES=1")
		}
	})

	t.Run("JUICE_ALLOW_LOCAL_SOURCES absent leaves config value", func(t *testing.T) {
		cfg := DefaultServerConfig()
		cfg.AllowLocalSources = true // from config.json
		applyEnvOverrides(&cfg)
		if !cfg.AllowLocalSources {
			t.Error("absent env must not override the config-file value")
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

// The peer transport binds OS-assigned ports unless the operator pins one. A pinned address is what
// lets peers find a kernel at the same place after it restarts; absent, nothing changes.
func TestFedListenAddrsIsReadFromConfig(t *testing.T) {
	if got := DefaultServerConfig().FedListenAddrs; len(got) != 0 {
		t.Fatalf("default pins %v; the default must be OS-assigned ports", got)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	want := DefaultServerConfig()
	want.FedListenAddrs = []string{"/ip4/127.0.0.1/tcp/31313"}
	b, _ := json.MarshalIndent(want, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.FedListenAddrs) != 1 || cfg.FedListenAddrs[0] != "/ip4/127.0.0.1/tcp/31313" {
		t.Errorf("fed_listen_addrs read back as %v", cfg.FedListenAddrs)
	}
}

// The money rules come from the kernel's own configuration and nowhere else, so a file that sets
// none of them gets the shipped economy whole. The three amounts are independent: choosing exact
// settlement (`lottery: 0`), or refusing every ticket (`lottery_max: 0`), must not quietly take the
// credit limit with it and stop the kernel serving foreign work at all.
func TestEconomyDefaultsAndIndependence(t *testing.T) {
	base := ServerConfig{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500}
	econ, err := base.Economy()
	if err != nil {
		t.Fatal(err)
	}
	if econ.Lottery != 1_000_000 || econ.LotteryMax != 5_000_000 || econ.CreditLimit != 500_000_000 {
		t.Errorf("unset money keys gave %d/%d/%d, want 1000000/5000000/500000000",
			econ.Lottery, econ.LotteryMax, econ.CreditLimit)
	}
	zero := int64(0)
	for _, c := range []struct {
		name string
		cfg  ServerConfig
	}{
		{"paying every obligation exactly", ServerConfig{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500, Lottery: &zero}},
		{"refusing every ticket", ServerConfig{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500, Lottery: &zero, LotteryMax: &zero}},
	} {
		got, err := c.cfg.Economy()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.CreditLimit != 500_000_000 {
			t.Errorf("%s moved the credit limit to %d", c.name, got.CreditLimit)
		}
	}
	// A kernel that would not accept its own ticket could never be paid for what it sells.
	five, four := int64(5), int64(4)
	if _, err := (ServerConfig{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500, Lottery: &five, LotteryMax: &four}).Economy(); err == nil {
		t.Error("a ticket above this kernel's own maximum was accepted")
	}
	// A negative amount is refused on its own account. Both cases below pass the size-against-maximum
	// check, so only the sign check can catch them.
	neg := int64(-1)
	for _, c := range []struct {
		name string
		cfg  ServerConfig
	}{
		{"lottery", ServerConfig{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500, Lottery: &neg}},
		{"credit_limit", ServerConfig{FeeBPS: 2000, RemoteBPS: 500, ImportBPS: 500, CreditLimit: &neg}},
	} {
		if _, err := c.cfg.Economy(); err == nil {
			t.Errorf("a negative %s was accepted", c.name)
		}
	}
}
