package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// instanceHome isolates an installation root and restores the served instance afterwards.
func instanceHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("JUICE_HOME", root)
	old := flagInstance
	flagInstance = defaultInstance
	t.Cleanup(func() { flagInstance = old })
	return root
}

// TestInstanceHomeIsPerKernel pins the layout: a kernel's whole home is one named directory under
// the installation root, so a second kernel is a sibling rather than a second installation.
func TestInstanceHomeIsPerKernel(t *testing.T) {
	root := instanceHome(t)
	if got, want := kernelHome(), filepath.Join(root, "kernels", "default"); got != want {
		t.Errorf("default instance: got %q, want %q", got, want)
	}
	flagInstance = "second"
	if got, want := kernelHome(), filepath.Join(root, "kernels", "second"); got != want {
		t.Errorf("named instance: got %q, want %q", got, want)
	}
	if got, want := cacheDir(), filepath.Join(root, "kernels", "second", "cache"); got != want {
		t.Errorf("cache: got %q, want %q", got, want)
	}
	// Client records belong to the installation, not to any one kernel.
	if got, want := clientHome(), filepath.Join(root, "client"); got != want {
		t.Errorf("client home: got %q, want %q", got, want)
	}
}

// TestInstanceNameValidation pins what may name a directory. The rules are the filesystem's: an
// instance label is not a handle and borrows no rule from the account namespace.
func TestInstanceNameValidation(t *testing.T) {
	for _, ok := range []string{"default", "second", "a-b_c.1", "PROD"} {
		if err := validateInstanceName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", "a@b", "a b", strings.Repeat("x", 65)} {
		if err := validateInstanceName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// seedLegacyHome writes a pre-instance kernel home: a database and the files that sit beside it.
func seedLegacyHome(t *testing.T) string {
	t.Helper()
	legacy := legacyKernelHome()
	if err := os.MkdirAll(filepath.Join(legacy, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"juice.db":    "LEDGER",
		"config.json": `{"world":"play"}`,
		"rail.key":    "RAILKEY",
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return legacy
}

// TestLegacyHomeMovesWholeAndOnce pins the kernel's own migration: the home moves entire — ledger,
// config, rail key and anything else beside them — in one rename, and a second boot finds nothing
// left to do. A ledger and a signing key are not worth a file-by-file protocol that can stop half
// way, and both paths share a root, so one rename is all it takes.
func TestLegacyHomeMovesWholeAndOnce(t *testing.T) {
	root := instanceHome(t)
	legacy := seedLegacyHome(t)

	if err := migrateLegacyHome(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dest := filepath.Join(root, "kernels", "default")
	for _, name := range []string{"juice.db", "config.json", "rail.key", "cache"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Errorf("%s did not move: %v", name, err)
		}
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "juice.db")); string(body) != "LEDGER" {
		t.Error("the ledger that moved is not the ledger that was there")
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Error("the old home is still in place, so the next boot would see two kernels")
	}
	// Idempotent: nothing left to move, and the moved kernel is untouched.
	if err := migrateLegacyHome(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "juice.db")); string(body) != "LEDGER" {
		t.Error("a second run disturbed the moved kernel")
	}
}

// TestLegacyHomeRefusesRatherThanMerges pins both refusals. A destination that already holds a
// kernel means two ledgers and only the operator can say which this installation serves; a home a
// server still holds must not be pulled out from under it.
func TestLegacyHomeRefusesRatherThanMerges(t *testing.T) {
	root := instanceHome(t)
	seedLegacyHome(t)
	dest := filepath.Join(root, "kernels", "default")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyHome(); err == nil {
		t.Fatal("a destination that already exists must be refused, never merged")
	}
	if _, err := os.Stat(filepath.Join(legacyKernelHome(), "juice.db")); err != nil {
		t.Error("the refused migration disturbed the old home")
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}

	// A running server holds the old home's lock.
	f, err := os.OpenFile(filepath.Join(legacyKernelHome(), "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyHome(); err == nil {
		t.Fatal("a home a server still holds must not be moved")
	}
	if _, err := os.Stat(filepath.Join(legacyKernelHome(), "juice.db")); err != nil {
		t.Error("the refused migration disturbed the old home")
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := migrateLegacyHome(); err != nil {
		t.Fatalf("once the server has stopped, the move proceeds: %v", err)
	}
}

// TestNoLegacyHomeIsNotAMigration: a fresh installation has nothing to move and says nothing.
func TestNoLegacyHomeIsNotAMigration(t *testing.T) {
	root := instanceHome(t)
	if err := migrateLegacyHome(); err != nil {
		t.Fatalf("fresh install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "kernels")); err == nil {
		t.Error("a migration that had nothing to do still created a home")
	}
}
