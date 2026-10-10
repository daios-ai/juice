// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/daios-ai/juice/rail"
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
	// A fresh kernel's language-model natives are bound to the shipped local endpoint (D17).
	if l := cfg.Native.LLM; l.Chat != "ollama/gemma" || l.JSON != "ollama/gemma" || l.Decide != "ollama/gemma" || l.Embed != "ollama/nomic" {
		t.Errorf("default llm bindings = %+v", cfg.Native.LLM)
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
	// The written-out file shows the operator the connection limits an ordinary kernel runs under,
	// so raising them for a kernel that carries the network is an edit, not a discovery.
	if cfg.MaxInboundPeers != 64 || cfg.RelaySlots != 128 {
		t.Errorf("connection limits default = %d inbound peers, %d relay slots; want 64, 128", cfg.MaxInboundPeers, cfg.RelaySlots)
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
	if err := os.WriteFile(path, []byte(`{"native":{"llm":{"chat":"anthropic/opus"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Native.LLM.Chat != "anthropic/opus" {
		t.Errorf("Native.LLM.Chat = %q", cfg.Native.LLM.Chat)
	}
	if cfg.Native.LLM.Embed != DefaultServerConfig().Native.LLM.Embed {
		t.Errorf("an unstated field must keep its default, got %q", cfg.Native.LLM.Embed)
	}
}

// A key the operator misspelled reads exactly like a key they never wrote, and the setting they
// meant to change silently keeps its default.
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
		cfg.Native.LLM.Chat = "from/file"
		cfg.FeeBPS = 1234
		applyEnvOverrides(&cfg)
		if cfg.LogLevel != "warn" {
			t.Errorf("LogLevel should not be overridden, got %q", cfg.LogLevel)
		}
		if cfg.Native.LLM.Chat != "from/file" {
			t.Errorf("Native.LLM.Chat should not be overridden, got %q", cfg.Native.LLM.Chat)
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
	econ, err := DefaultServerConfig().Economy()
	if err != nil {
		t.Fatal(err)
	}
	if econ != kernel.DefaultEconomy() {
		t.Errorf("the default configuration gave %+v, want the shipped economy %+v", econ, kernel.DefaultEconomy())
	}
	with := func(edit func(*ServerConfig)) ServerConfig {
		c := DefaultServerConfig()
		edit(&c)
		return c
	}
	for _, c := range []struct {
		name string
		cfg  ServerConfig
	}{
		{"paying every obligation exactly", with(func(c *ServerConfig) { c.Lottery = 0 })},
		{"refusing every ticket", with(func(c *ServerConfig) { c.Lottery, c.LotteryMax = 0, 0 })},
	} {
		got, err := c.cfg.Economy()
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.CreditLimit != 50_000_000 {
			t.Errorf("%s moved the credit limit to %d", c.name, got.CreditLimit)
		}
	}
	// A kernel that would not accept its own ticket could never be paid for what it sells.
	if _, err := with(func(c *ServerConfig) { c.Lottery, c.LotteryMax = 5, 4 }).Economy(); err == nil {
		t.Error("a ticket above this kernel's own maximum was accepted")
	}
	// A negative amount is refused on its own account. Both cases below pass the size-against-maximum
	// check, so only the sign check can catch them.
	for name, cfg := range map[string]ServerConfig{
		"lottery":      with(func(c *ServerConfig) { c.Lottery = -1 }),
		"credit_limit": with(func(c *ServerConfig) { c.CreditLimit = -1 }),
		"fee_bps":      with(func(c *ServerConfig) { c.FeeBPS = 10001 }),
	} {
		if _, err := cfg.Economy(); err == nil {
			t.Errorf("an out-of-range %s was accepted", name)
		}
	}
}

// testHome isolates an installation root and restores the served world afterwards.
func testHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("JUICE_HOME", root)
	old := worldName
	worldName = "play"
	t.Cleanup(func() { worldName = old })
	return root
}

// TestInstanceHomeIsPerWorld pins the layout: a kernel's whole home is one directory named for the
// world it serves, so a kernel on a second network is a sibling rather than a second installation,
// while the worlds themselves and the client's records belong to the installation.
func TestInstanceHomeIsPerWorld(t *testing.T) {
	root := testHome(t)
	if got, want := kernelHome(), filepath.Join(root, "kernels", "play"); got != want {
		t.Errorf("served world: got %q, want %q", got, want)
	}
	worldName = "arbitrum-one"
	if got, want := kernelHome(), filepath.Join(root, "kernels", "arbitrum-one"); got != want {
		t.Errorf("served world: got %q, want %q", got, want)
	}
	if got, want := cacheDir(), filepath.Join(root, "kernels", "arbitrum-one", "cache"); got != want {
		t.Errorf("cache: got %q, want %q", got, want)
	}
	if got, want := worldsDir(), filepath.Join(root, "worlds"); got != want {
		t.Errorf("worlds: got %q, want %q", got, want)
	}
	// Client records belong to the installation, not to any one kernel.
	if got, want := clientHome(), filepath.Join(root, "client"); got != want {
		t.Errorf("client home: got %q, want %q", got, want)
	}
}

// TestKernelNameValidation pins what may name a world on this machine. The name becomes a file
// under worlds/ and a directory under kernels/, so it is one path segment and nothing else.
func TestKernelNameValidation(t *testing.T) {
	for _, ok := range []string{"acme", "second", "a-b_c.1", "PROD"} {
		if err := validateLocalName("world", ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", "a@b", "a b", strings.Repeat("x", 65)} {
		if err := validateLocalName("world", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestKernelsHereNamesOnlyRealKernels: the list an operator is shown when a name is not recognised
// must be true, so a directory holding no database — a first boot that was answered and then
// abandoned — is not one of this installation's kernels.
func TestKernelsHereNamesOnlyRealKernels(t *testing.T) {
	root := testHome(t)
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(root, "kernels", name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "kernels", "alpha", "juice.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kernels", "stray"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := kernelsHere(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("kernels here: got %v, want [alpha]", got)
	}
}

// ---- Command-line overrides ----

// Every setting of the configuration file is settable on the command line, because the flags are
// derived from the struct: a setting added later gets its flag without anyone remembering to add
// one. The exceptions are the credentials key and the language-model endpoints' keys, which would
// otherwise stand in the process table for every user of the machine to read.
func TestEveryConfigKeyHasAFlag(t *testing.T) {
	fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	holder := DefaultServerConfig()
	bindConfigFlags(fs, &holder)

	var missing []string
	configFields(&holder, func(name string, _ reflect.Value) {
		if name == "credentials-key" || name == "native.llm.endpoints" {
			if fs.Lookup(name) != nil {
				t.Errorf("%s is settable on the command line, where the machine can read it", name)
			}
			return
		}
		if fs.Lookup(name) == nil {
			missing = append(missing, name)
		}
	})
	if len(missing) > 0 {
		t.Fatalf("settings with no flag: %s", strings.Join(missing, ", "))
	}
	// A spot check that the spelling is the key's, so the operator reads one vocabulary.
	for _, name := range []string{"listen-addr", "fed-listen-addrs", "fee-bps", "native.llm.chat", "native.lookup.default-limit", "max-inbound-peers", "relay-slots"} {
		if fs.Lookup(name) == nil {
			t.Errorf("no flag named %s", name)
		}
	}
}

// What the command line says wins over the file, for the settings named on it and no others.
func TestCommandLineWinsOverTheFile(t *testing.T) {
	file := DefaultServerConfig()
	file.ListenAddr = ":9999"
	file.FeeBPS = 1234
	file.KernelHandle = "acme"
	file.AllowLocalSources = true
	file.Native.LLM.Chat = "file/chat"
	file.Native.Lookup.DefaultLimit = 7
	file.FedListenAddrs = []string{"/ip4/0.0.0.0/tcp/1"}
	file.Lottery = 11

	fs := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	serveOverride = DefaultServerConfig()
	bindConfigFlags(fs, &serveOverride)
	serveFlags = fs
	t.Cleanup(func() { serveFlags = nil })
	if err := fs.Parse([]string{"--listen-addr", ":4141", "--fee-bps", "500",
		"--allow-local-sources=false", "--native.llm.chat", "flag/chat",
		"--native.lookup.default-limit", "3", "--fed-listen-addrs", "/ip4/0.0.0.0/tcp/2",
		"--lottery", "22"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := file
	applyConfigFlags(&got)
	if got.ListenAddr != ":4141" || got.FeeBPS != 500 || got.AllowLocalSources {
		t.Fatalf("plain settings not overridden: %+v", got)
	}
	if got.Native.LLM.Chat != "flag/chat" || got.Native.Lookup.DefaultLimit != 3 {
		t.Fatalf("nested settings not overridden: %+v", got.Native)
	}
	if len(got.FedListenAddrs) != 1 || got.FedListenAddrs[0] != "/ip4/0.0.0.0/tcp/2" {
		t.Fatalf("list setting not overridden: %v", got.FedListenAddrs)
	}
	if got.Lottery != 22 {
		t.Fatalf("money setting not overridden: %v", got.Lottery)
	}
	// Untyped on that command line, so the file still decides it.
	if got.KernelHandle != "acme" {
		t.Fatalf("a setting nobody typed was overwritten: kernel_handle = %q", got.KernelHandle)
	}
	// And the file itself is unchanged: an override lasts for the run, not for the kernel.
	if file.ListenAddr != ":9999" || file.FeeBPS != 1234 {
		t.Fatalf("the loaded configuration was mutated: %+v", file)
	}
}

// With no serve command in play — every other command, and every test that does not parse flags —
// the configuration is the file's alone.
func TestNoFlagsLeavesTheConfigurationAlone(t *testing.T) {
	serveFlags = nil
	cfg := DefaultServerConfig()
	cfg.FeeBPS = 4321
	applyConfigFlags(&cfg)
	if cfg.FeeBPS != 4321 {
		t.Fatalf("configuration changed with no flags parsed: %d", cfg.FeeBPS)
	}
}

// llmEndpoints is a pair of endpoint files as Load reads them: one local and free, one metered.
func llmEndpoints() map[string]llm.Endpoint {
	return map[string]llm.Endpoint{
		"local": {Protocol: llm.ProtocolOpenAI, URL: "http://localhost:1/v1", Models: map[string]llm.Model{
			"chat": {ID: "c", Kind: llm.KindChat}, "vec": {ID: "v", Kind: llm.KindEmbed}}},
		"cloud": {Protocol: llm.ProtocolAnthropic, URL: "https://x/v1", KeyRequired: true, Models: map[string]llm.Model{
			"big": {ID: "b", Kind: llm.KindChat}, "small": {ID: "s", Kind: llm.KindChat}}},
	}
}

// A configuration that cannot mean what it says is refused before anything is served, naming the
// key to correct (D17); one that can is accepted, a metered endpoint stating every model's price.
func TestLLMConfigCheck(t *testing.T) {
	ok := NativeLLMConfig{Chat: "cloud/big", Decide: "local/chat", Embed: "local/vec", Endpoints: map[string]LLMEndpointConfig{
		"cloud": {Key: "k", Prices: map[string]int64{"big": 20, "small": 0}}}}
	if err := ok.check(llmEndpoints()); err != nil {
		t.Fatalf("a sound configuration was refused: %v", err)
	}
	if err := (NativeLLMConfig{}).check(llmEndpoints()); err != nil {
		t.Fatalf("an unbound configuration was refused: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		edit       func(*NativeLLMConfig)
	}{
		{"unknown model", "native.llm.chat", func(c *NativeLLMConfig) { c.Chat = "local/none" }},
		{"unknown endpoint", "native.llm.decide", func(c *NativeLLMConfig) { c.Decide = "nowhere/chat" }},
		{"chat bound to an embed model", "native.llm.chat", func(c *NativeLLMConfig) { c.Chat = "local/vec" }},
		{"embed bound to a chat model", "native.llm.embed", func(c *NativeLLMConfig) { c.Embed = "local/chat" }},
		{"json bound to an embed model", "native.llm.json", func(c *NativeLLMConfig) { c.JSON = "local/vec" }},
		{"bound endpoint lacks its key", "native.llm.endpoints.cloud.key", func(c *NativeLLMConfig) {
			c.Endpoints = map[string]LLMEndpointConfig{"cloud": {Prices: map[string]int64{"big": 1, "small": 1}}}
		}},
		{"settings for no endpoint", "native.llm.endpoints.ghost", func(c *NativeLLMConfig) {
			c.Endpoints["ghost"] = LLMEndpointConfig{}
		}},
		{"price for no model", "native.llm.endpoints.cloud.prices.huge", func(c *NativeLLMConfig) {
			c.Endpoints["cloud"].Prices["huge"] = 1
		}},
		{"negative price", "must not be negative", func(c *NativeLLMConfig) { c.Endpoints["cloud"].Prices["big"] = -1 }},
		{"metered model without a price", "native.llm.endpoints.cloud.prices.small", func(c *NativeLLMConfig) {
			delete(c.Endpoints["cloud"].Prices, "small")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			c.Endpoints = map[string]LLMEndpointConfig{"cloud": {Key: "k", Prices: map[string]int64{"big": 20, "small": 0}}}
			tc.edit(&c)
			if err := c.check(llmEndpoints()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// A language-model native costs what the model behind it does: the canonical natives their bound
// model's price, a generated model's natives its own, and an unbound native nothing.
func TestLLMNativePrices(t *testing.T) {
	c := NativeConfig{LLM: NativeLLMConfig{Chat: "cloud/big", JSON: "cloud/small", Decide: "cloud/small", Endpoints: map[string]LLMEndpointConfig{
		"cloud": {Key: "k", Prices: map[string]int64{"big": 20, "small": 3}}}}}
	for name, want := range map[string]int64{
		"llm/chat": 20, "llm/json": 3, "llm/decide": 3, "llm/embed": 0,
		"llm/cloud/big/chat": 20, "llm/cloud/big/json": 20, "llm/cloud/big/decide": 20, "llm/cloud/small/chat": 3, "llm/local/vec/embed": 0,
	} {
		if got := c.PriceOf(name); got != want {
			t.Errorf("PriceOf(%s) = %d, want %d", name, got, want)
		}
	}
}

// The generator maps each kind it supports, admits null where Go decodes it, carries descriptions,
// allowed values and non-zero defaults, and skips a field tagged "-".
func TestJSONSchemaKinds(t *testing.T) {
	type inner struct {
		X float64 `json:"x" doc:"d"`
	}
	type probe struct {
		S    string           `json:"s" doc:"a string" enum:"a,b"`
		N    int64            `json:"n" doc:"d"`
		B    bool             `json:"b" doc:"d"`
		L    []string         `json:"l" doc:"d"`
		M    map[string]inner `json:"m" doc:"d"`
		In   inner            `json:"in" doc:"d"`
		Ask  string           `json:"ask" doc:"d" default:"-"`
		Skip string           `json:"-"`
	}
	got, err := jsonSchema("probe", probe{S: "a", N: 7})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	var s struct {
		Title                string                     `json:"title"`
		AdditionalProperties bool                       `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		// Every setting states its default, zero included; none is nullable.
		"s": `{"default":"a","description":"a string","enum":["a","b"],"type":"string"}`,
		"n": `{"default":7,"description":"d","type":"integer"}`,
		"b": `{"default":false,"description":"d","type":"boolean"}`,
		"l": `{"default":[],"description":"d","items":{"type":"string"},"type":"array"}`,
		// A map entry is the author's, so its fields carry no default.
		"m":   `{"additionalProperties":{"additionalProperties":false,"properties":{"x":{"description":"d","type":"number"}},"type":"object"},"default":{},"description":"d","properties":{},"type":"object"}`,
		"in":  `{"additionalProperties":false,"description":"d","properties":{"x":{"default":0,"description":"d","type":"number"}},"type":"object"}`,
		"ask": `{"description":"d","type":"string"}`,
	}
	if s.Title != "probe" || s.AdditionalProperties || len(s.Properties) != len(want) {
		t.Fatalf("schema = %s", b)
	}
	for key, w := range want {
		if string(s.Properties[key]) != w {
			t.Errorf("%s = %s, want %s", key, s.Properties[key], w)
		}
	}
}

// A field the generator cannot describe is an error, never a silent gap: Go would decode an
// untagged field under its Go name, and a setting without a description would ship unexplained.
func TestJSONSchemaRefusesWhatItCannotDescribe(t *testing.T) {
	for name, v := range map[string]any{
		"untagged": struct{ X string }{},
		"no doc": struct {
			X string `json:"x"`
		}{},
		"unsupported": struct {
			C chan int `json:"c" doc:"d"`
		}{},
		"map key": struct {
			M map[int]string `json:"m" doc:"d"`
		}{},
		// A pointer says null, which a setting never means: absent keeps the default.
		"pointer": struct {
			P *int64 `json:"p" doc:"d"`
		}{},
		// A default the schema itself refuses is caught by the kernel's own check (D4).
		"lying default": struct {
			S string `json:"s" doc:"d" enum:"a,b"`
		}{},
	} {
		if _, err := jsonSchema("x", v); err == nil {
			t.Errorf("%s: a schema was generated", name)
		}
	}
}

// Each file an operator writes gets a schema in $JUICE_HOME/schemas/, and Juice's own files are
// valid under it: the config.json first boot writes and every shipped world and
// endpoint file. Each names its schema in $schema, which the strict decoders accept and which is
// no setting, so it has no flag.
func TestOperatorFilesValidateAgainstTheirSchemas(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JUICE_HOME", home)
	if err := writeSchemas(); err != nil {
		t.Fatal(err)
	}
	schema := func(name string) map[string]any {
		var s map[string]any
		raw, err := os.ReadFile(filepath.Join(schemasDir(), name+".schema.json"))
		if err != nil || json.Unmarshal(raw, &s) != nil {
			t.Fatalf("%s schema unreadable: %v", name, err)
		}
		return s
	}
	valid := func(what string, s map[string]any, raw []byte) {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if err := kernel.ValidateInput(s, doc); err != nil {
			t.Errorf("%s is not valid under its own schema: %v", what, err)
		}
	}

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := writeConfig(cfgPath, DefaultServerConfig()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfgPath)
	valid("the first-boot config.json", schema("config"), raw)
	cfg, err := LoadConfig(cfgPath)
	if err != nil || cfg.Schema != "../../schemas/config.schema.json" {
		t.Errorf("config.json with $schema: %q, %v", cfg.Schema, err)
	}
	flags := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	bindConfigFlags(flags, &ServerConfig{})
	if flags.Lookup("$schema") != nil {
		t.Error("$schema became a flag")
	}

	for name, files := range map[string]fs.FS{"world": rail.Worlds(), "llm-endpoint": llm.Shipped()} {
		s := schema(name)
		entries, _ := fs.ReadDir(files, ".")
		for _, e := range entries {
			raw, _ := fs.ReadFile(files, e.Name())
			valid(name+" "+e.Name(), s, raw)
		}
	}

	// Each schema is in the kernel's canonical subset (D4), as a form reads it.
	for _, name := range []string{"config", "world", "llm-endpoint"} {
		s := schema(name)
		delete(s, "$schema") // the one keyword a schema file carries that a schema node does not
		if err := kernel.ValidateSchema(name, s); err != nil {
			t.Errorf("%s schema: %v", name, err)
		}
	}
	// A form shows every setting's default, so every setting states one except the two first boot
	// asks for or mints; and the file first boot writes says every value, none of them null.
	var missing []string
	var walk func(s map[string]any, path string)
	walk = func(s map[string]any, path string) {
		props, _ := s["properties"].(map[string]any)
		for k, v := range props {
			child := v.(map[string]any)
			if child["type"] == "object" && child["additionalProperties"] == false {
				walk(child, path+k+".")
			} else if _, ok := child["default"]; !ok {
				missing = append(missing, path+k)
			}
		}
	}
	walk(schema("config"), "")
	if sort.Strings(missing); strings.Join(missing, " ") != "credentials_key kernel_handle" {
		t.Errorf("settings without a default: %v", missing)
	}
	if strings.Contains(string(raw), "null") {
		t.Errorf("the first-boot config.json holds a null:\n%s", raw)
	}
	// A file written before every value was explicit holds null for four keys; it still loads,
	// each keeping its default.
	old := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(old, []byte(`{"lottery": null, "lottery_max": null, "credit_limit": null, "fed_listen_addrs": null}`), 0o600)
	if cfg, err := LoadConfig(old); err != nil {
		t.Errorf("an older config.json with nulls: %v", err)
	} else if econ, _ := cfg.Economy(); econ != kernel.DefaultEconomy() {
		t.Errorf("an older config.json with nulls gave %+v", econ)
	}
}
