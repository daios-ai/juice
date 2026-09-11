package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// testHome isolates an installation root and restores the served kernel afterwards.
func testHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("JUICE_HOME", root)
	old := kernelName
	kernelName = "acme"
	t.Cleanup(func() { kernelName = old })
	return root
}

// TestInstanceHomeIsPerKernel pins the layout: a kernel's whole home is one named directory under
// the installation root, so a second kernel is a sibling rather than a second installation.
func TestInstanceHomeIsPerKernel(t *testing.T) {
	root := testHome(t)
	if got, want := kernelHome(), filepath.Join(root, "kernels", "acme"); got != want {
		t.Errorf("named kernel: got %q, want %q", got, want)
	}
	kernelName = "second"
	if got, want := kernelHome(), filepath.Join(root, "kernels", "second"); got != want {
		t.Errorf("named kernel: got %q, want %q", got, want)
	}
	if got, want := cacheDir(), filepath.Join(root, "kernels", "second", "cache"); got != want {
		t.Errorf("cache: got %q, want %q", got, want)
	}
	// Client records belong to the installation, not to any one kernel.
	if got, want := clientHome(), filepath.Join(root, "client"); got != want {
		t.Errorf("client home: got %q, want %q", got, want)
	}
}

// TestKernelNameValidation pins what may name a kernel. The name is both the nickname the network
// carries and this machine's directory for it, so it must satisfy both: bare, as every handle is,
// and one path segment, as every label under this root is.
func TestKernelNameValidation(t *testing.T) {
	for _, ok := range []string{"acme", "second", "a-b_c.1", "PROD"} {
		if err := validateLocalName("kernel", ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", "a@b", "a b", strings.Repeat("x", 65)} {
		if err := validateLocalName("kernel", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// seedLegacyHome writes an unnamed legacy kernel home: a database and the files that sit beside it.
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
	root := testHome(t)
	legacy := seedLegacyHome(t)

	if err := migrateLegacyHome(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dest := filepath.Join(root, "kernels", "acme")
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
	root := testHome(t)
	seedLegacyHome(t)
	dest := filepath.Join(root, "kernels", "acme")
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
	root := testHome(t)
	if err := migrateLegacyHome(); err != nil {
		t.Fatalf("fresh install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "kernels")); err == nil {
		t.Error("a migration that had nothing to do still created a home")
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
