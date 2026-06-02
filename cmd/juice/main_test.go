package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvOr(t *testing.T) {
	const key = "JUICE_TEST_VAR_ENVTEST"
	os.Unsetenv(key)
	if got := envOr(key, "default"); got != "default" {
		t.Errorf("envOr unset: got %q, want %q", got, "default")
	}
	os.Setenv(key, "custom")
	t.Cleanup(func() { os.Unsetenv(key) })
	if got := envOr(key, "default"); got != "custom" {
		t.Errorf("envOr set: got %q, want %q", got, "custom")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	if err := saveToken("tok123"); err != nil {
		t.Fatal(err)
	}
	got, err := loadToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "tok123" {
		t.Errorf("loadToken: got %q, want %q", got, "tok123")
	}
	if err := removeToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken(); err == nil {
		t.Error("expected error after removeToken")
	}
}

func TestRefreshTokenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	if err := saveRefreshToken("rt456"); err != nil {
		t.Fatal(err)
	}
	got, err := loadRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "rt456" {
		t.Errorf("loadRefreshToken: got %q, want %q", got, "rt456")
	}
	if err := removeRefreshToken(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRefreshToken(); err == nil {
		t.Error("expected error after removeRefreshToken")
	}
}

func TestLoadConfigFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("sets missing vars from file", func(t *testing.T) {
		path := filepath.Join(dir, "cfg_a.json")
		os.WriteFile(path, []byte(`{"JUICE_TEST_B3_KEY_A":"val_a"}`), 0o600)
		t.Setenv("JUICE_CONFIG_FILE", path)
		os.Unsetenv("JUICE_TEST_B3_KEY_A")
		t.Cleanup(func() { os.Unsetenv("JUICE_TEST_B3_KEY_A") })

		if err := loadConfigFile(); err != nil {
			t.Fatalf("loadConfigFile: %v", err)
		}
		if got := os.Getenv("JUICE_TEST_B3_KEY_A"); got != "val_a" {
			t.Errorf("got %q, want %q", got, "val_a")
		}
	})

	t.Run("does not override existing env vars", func(t *testing.T) {
		path := filepath.Join(dir, "cfg_b.json")
		os.WriteFile(path, []byte(`{"JUICE_TEST_B3_KEY_B":"from_file"}`), 0o600)
		t.Setenv("JUICE_CONFIG_FILE", path)
		t.Setenv("JUICE_TEST_B3_KEY_B", "from_env")

		if err := loadConfigFile(); err != nil {
			t.Fatalf("loadConfigFile: %v", err)
		}
		if got := os.Getenv("JUICE_TEST_B3_KEY_B"); got != "from_env" {
			t.Errorf("got %q, want %q (env var must not be overridden)", got, "from_env")
		}
	})

	t.Run("missing file is not an error", func(t *testing.T) {
		t.Setenv("JUICE_CONFIG_FILE", filepath.Join(dir, "nonexistent.json"))
		if err := loadConfigFile(); err != nil {
			t.Errorf("missing config file should not be an error: %v", err)
		}
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		os.WriteFile(path, []byte(`not valid json`), 0o600)
		t.Setenv("JUICE_CONFIG_FILE", path)
		if err := loadConfigFile(); err == nil {
			t.Error("expected error for invalid JSON config file")
		}
	})

	t.Run("numeric values are converted to string", func(t *testing.T) {
		path := filepath.Join(dir, "cfg_num.json")
		os.WriteFile(path, []byte(`{"JUICE_TEST_B3_KEY_C":2000}`), 0o600)
		t.Setenv("JUICE_CONFIG_FILE", path)
		os.Unsetenv("JUICE_TEST_B3_KEY_C")
		t.Cleanup(func() { os.Unsetenv("JUICE_TEST_B3_KEY_C") })

		if err := loadConfigFile(); err != nil {
			t.Fatalf("loadConfigFile: %v", err)
		}
		if got := os.Getenv("JUICE_TEST_B3_KEY_C"); got != "2000" {
			t.Errorf("got %q, want %q", got, "2000")
		}
	})
}
