package main

import (
	"os"
	"strings"
	"testing"
)

// TestKernelAddNamesAndPinsWithoutSelecting: registering a kernel records where it answers and the
// identity that answered, under the nickname it advertises unless the operator gives a name. It
// selects nothing — where you are is a login, and there is none yet.
func TestKernelAddNamesAndPinsWithoutSelecting(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")

	out, err := execTestCmd(t, kernelAddCmd(), srv.URL)
	if err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	cfg := loadClientConfig()
	k := cfg.Kernels["k"] // healthServer advertises the nickname "k"
	if k == nil || k.Endpoint != srv.URL || k.PublicKey != "KEY-A" || k.Network != "play" {
		t.Fatalf("not recorded under its nickname: %+v", cfg.Kernels)
	}
	if cfg.Current != "" {
		t.Errorf("adding a kernel selected %q", cfg.Current)
	}

	// A name of the operator's own is taken as given.
	if _, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work"); err != nil {
		t.Fatalf("add under a name: %v", err)
	}
	if loadClientConfig().Kernels["work"] == nil {
		t.Error("the given name was not used")
	}
}

// TestKernelAddIsIdempotentButNotACoup: adding a kernel already known, on the same terms, changes
// nothing and succeeds — a harness may register before every login. A different kernel under a name
// already held is refused, because a nickname is a label and not a proof of anything.
func TestKernelAddIsIdempotentButNotACoup(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	if _, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work"); err != nil {
		t.Fatal(err)
	}
	recordLogin(t, "alice@work", srv.URL, "KEY-A")
	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work")
		return err
	})
	if !strings.Contains(out, "already known") {
		t.Errorf("a repeat add must say it changed nothing: %q", out)
	}

	other := healthServer(t, "KEY-B", "DIGEST-A", "play")
	resetHealthCache()
	if _, err := execTestCmd(t, kernelAddCmd(), other.URL, "work"); err == nil {
		t.Fatal("another kernel took a name already held")
	}
	if got := loadClientConfig().Kernels["work"]; got.PublicKey != "KEY-A" {
		t.Errorf("the refused add changed the record: %+v", got)
	}

	// The same kernel at a new address is that kernel: a restart on another port keeps its name and
	// its logins, because a session belongs to whoever issued it and the key says that is this one.
	if err := saveToken("SECRET"); err != nil {
		t.Fatal(err)
	}
	moved := healthServer(t, "KEY-A", "DIGEST-A", "play")
	resetHealthCache()
	if _, err := execTestCmd(t, kernelAddCmd(), moved.URL, "work"); err != nil {
		t.Fatalf("a kernel that moved was refused its own name: %v", err)
	}
	if got := loadClientConfig().Kernels["work"]; got.Endpoint != moved.URL {
		t.Errorf("the new address was not recorded: %+v", got)
	}
	if tok, err := loadToken(); err != nil || tok != "SECRET" {
		t.Errorf("a move logged the operator out: %q %v", tok, err)
	}
}

// TestKernelForgetTakesTheLoginsWithIt: a client that no longer knows a kernel holds no credential
// that could be sent to it, and is not left selected on it. Nothing of the kernel's own is touched.
func TestKernelForgetTakesTheLoginsWithIt(t *testing.T) {
	clientHomeFor(t)
	recordLogin(t, "alice@work", "http://kernel:4040", "KEY")
	if err := saveToken("SECRET"); err != nil {
		t.Fatal(err)
	}
	if _, err := execTestCmd(t, kernelForgetCmd(), "work", "--yes"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	cfg := loadClientConfig()
	if cfg.Kernels["work"] != nil {
		t.Error("the record survived")
	}
	if cfg.Current != "" {
		t.Errorf("still selected: %q", cfg.Current)
	}
	if _, err := os.Stat(credPath(t, "alice@work")); !os.IsNotExist(err) {
		t.Errorf("the credential survived: %v", err)
	}
	if _, err := execTestCmd(t, kernelForgetCmd(), "work", "--yes"); err == nil {
		t.Error("forgetting an unknown kernel must be refused")
	}
}

// TestKernelListMarksWhereYouAre: the list is what this client knows, and the mark is the kernel
// the selected login acts through — the one answer an operator with two kernels needs.
func TestKernelListMarksWhereYouAre(t *testing.T) {
	clientHomeFor(t)
	cfg := loadClientConfig()
	cfg.Kernels["work"] = &kernelRec{Endpoint: "http://work:4040", PublicKey: "KEY-W", Network: "play"}
	cfg.Kernels["lab"] = &kernelRec{Endpoint: "http://lab:4040", PublicKey: "KEY-L", Network: "test"}
	cfg.Current = "alice@lab"
	if err := saveClientConfig(cfg); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, kernelListCmd())
		return err
	})
	for _, want := range []string{"KERNEL", "work", "lab", "http://lab:4040", "KEY-W"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing omits %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "lab") && !strings.HasPrefix(line, "*") {
			t.Errorf("the selected kernel is not marked: %q", line)
		}
		if strings.Contains(line, "work") && strings.HasPrefix(line, "*") {
			t.Errorf("an unselected kernel is marked: %q", line)
		}
	}
}

// TestKernelHealthNeedsNoLogin: what a server says about itself is public, and reading it is how a
// client decides whether to trust an address at all — so it works before anyone has logged in.
func TestKernelHealthNeedsNoLogin(t *testing.T) {
	clientHomeFor(t)
	srv := healthServer(t, "KEY-A", "DIGEST-A", "play")
	if _, err := execTestCmd(t, kernelAddCmd(), srv.URL, "work"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error {
		_, err := execTestCmd(t, kernelHealthCmd(), "work")
		return err
	})
	if !strings.Contains(out, "network play") {
		t.Errorf("health does not report the network: %q", out)
	}
	if _, err := execTestCmd(t, kernelHealthCmd(), "nosuch"); err == nil {
		t.Error("health on an unknown kernel must be refused")
	}
}
