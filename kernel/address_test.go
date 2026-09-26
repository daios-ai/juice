// SPDX-License-Identifier: AGPL-3.0-only

package kernel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/store"
)

// An address is handle@kernel[/name]; anything else — a bare handle, owner/name, a sigil, an empty
// half, whitespace — is refused (D15).
func TestParseAddress(t *testing.T) {
	for _, c := range []struct{ in, handle, kernel, name string }{
		{"tom@k", "tom", "k", ""},
		{"tom@k/brief", "tom", "k", "brief"},
		{"tom@k/brief/eu", "tom", "k", "brief/eu"},
		{"tom@k/mail@home", "tom", "k", "mail@home"}, // only the head qualifies a kernel
		{" tom@k/brief ", "tom", "k", "brief"},
	} {
		a, err := kernel.ParseAddress(c.in)
		if err != nil || a.Handle != c.handle || a.Kernel != c.kernel || a.Name != c.name {
			t.Errorf("ParseAddress(%q) = (%+v,%v), want (%q,%q,%q)", c.in, a, err, c.handle, c.kernel, c.name)
		}
		if a.String() != strings.TrimSpace(c.in) {
			t.Errorf("String() = %q, want %q", a.String(), strings.TrimSpace(c.in))
		}
	}
	for _, bad := range []string{"tom", "tom/brief", "@k", "tom@", "@", "tom@k/", "to m@k", "tom@k k", ""} {
		if _, err := kernel.ParseAddress(bad); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("ParseAddress(%q): want ErrInvalidInput, got %v", bad, err)
		}
	}
}

// A kernel's own name is a petname bound to its own key: it resolves as this kernel by name and by
// key, no peer can be given it, a peer arriving with the same nickname is suffixed away from it,
// the kernel's own commands refuse it as a peer, and the stale-discovery sweep never evicts it.
func TestOwnNameIsAPetnameOnTheOwnKey(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)
	self := k.SelfKeyForTest(ctx)

	for _, seg := range []string{kernel.TestOwnName, self} {
		kr, err := k.ResolveKernel(ctx, seg)
		if err != nil || !kr.Local || kr.Key != self {
			t.Fatalf("ResolveKernel(%q) = %+v, %v; want this kernel", seg, kr, err)
		}
	}
	if _, err := k.BindPetname(ctx, testKernelKey(3), kernel.TestOwnName, true); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("a peer took this kernel's own name: %v", err)
	}
	if err := k.ObserveKernel(ctx, testKernelKey(4), kernel.TestOwnName, ""); err != nil {
		t.Fatal(err)
	}
	if got, err := k.BindPetname(ctx, testKernelKey(4), "", false); err != nil || got == kernel.TestOwnName || !strings.HasPrefix(got, kernel.TestOwnName+"-") {
		t.Errorf("an auto-bound peer with our nickname got %q, %v; want it suffixed away", got, err)
	}
	if _, err := k.RenameKernel(ctx, sys.ID, self, "other"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("renaming this kernel as a peer: want ErrInvalidInput, got %v", err)
	}
	if _, err := k.EnsureKernelAccount(ctx, self); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("an account with itself: want ErrInvalidInput, got %v", err)
	}

	// The sweep past the retention age evicts account-less rows, which the self row is by design.
	if err := st.UpsertKernel(ctx, self, "", "", "", "", time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.(*store.DB).PurgeStaleDiscovery(ctx, time.Now(), self); err != nil {
		t.Fatal(err)
	}
	if got := k.OwnName(ctx); got != kernel.TestOwnName {
		t.Fatalf("the sweep took this kernel's own name: %q", got)
	}
	if kr, err := k.ResolveKernel(ctx, kernel.TestOwnName); err != nil || !kr.Local {
		t.Errorf("after the sweep the own name no longer resolves: %+v, %v", kr, err)
	}

	// A boot whose name a peer already holds is refused with the remedy in it.
	if _, err := k.BindPetname(ctx, testKernelKey(5), "taken", true); err != nil {
		t.Fatal(err)
	}
	err := k.BindOwnName(ctx, "taken")
	if !errors.Is(err, kernel.ErrInvalidState) || !strings.Contains(err.Error(), "admin peer rename") {
		t.Errorf("boot under a peer's name: want ErrInvalidState naming the remedy, got %v", err)
	}
}

// A reference is remote unless it names this kernel: a peer, or a kernel nobody here knows, is not
// answered here, so sys/llm/decide discards such a candidate instead of aborting (U46). A malformed
// reference is not remote; the resolver refuses it as bad input.
func TestIsRemoteRef(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	if _, err := k.BindPetname(ctx, testKernelKey(9), "peer", true); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]bool{
		"alice@k/x":                             false,
		"alice@" + k.SelfKeyForTest(ctx) + "/x": false,
		"alice@peer/x":                          true,
		"alice@nowhere/x":                       true,
		"alice@" + testKernelKey(10) + "/x":     true,
		"alice/x":                               false,
	} {
		if got := k.IsRemoteRef(ctx, ref); got != want {
			t.Errorf("IsRemoteRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

// Every party renders as an address (D20): a user here beneath the own name, a peer's user beneath
// the peer's name, the peer kernel itself as its name alone, an unbound peer as its key, and only a
// purged tombstone as its raw id. An action's address is its owner's plus the name, a proxy's the
// remote owner's beneath the peer.
func TestNamesRenderPrincipalsAndActions(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	alice := setupUser(t, st, "alice", 0)
	named, err := k.EnsureKernelAccount(ctx, testKernelKey(6))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.BindPetname(ctx, testKernelKey(6), "peer", true); err != nil {
		t.Fatal(err)
	}
	unbound, err := k.EnsureKernelAccount(ctx, testKernelKey(7))
	if err != nil {
		t.Fatal(err)
	}
	names := k.NewNames()
	for _, c := range []struct {
		p    kernel.Principal
		want string
	}{
		{kernel.Principal{AccountID: alice.ID}, "alice@k"},
		{kernel.Principal{AccountID: named.ID}, "peer"},
		{kernel.Principal{AccountID: named.ID, RemoteID: "u-1", Handle: "bob"}, "bob@peer"},
		{kernel.Principal{AccountID: named.ID, RemoteID: "u-1"}, "u-1@peer"},
		{kernel.Principal{AccountID: unbound.ID}, testKernelKey(7)},
		{kernel.Principal{AccountID: "gone"}, "gone"},
		{kernel.Principal{}, ""},
	} {
		if got := names.Address(ctx, c.p); got != c.want {
			t.Errorf("Address(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
	local := &kernel.Action{OwnerUserID: alice.ID, Name: "greet"}
	proxy := &kernel.Action{OwnerUserID: named.ID, Name: kernel.JoinProxyName("bob", "greet"), Kind: kernel.KindRemoteProxy, RemoteOwnerID: "u-1"}
	if got := names.Action(ctx, local); got != "alice@k/greet" {
		t.Errorf("local action = %q", got)
	}
	if got := names.Action(ctx, proxy); got != "bob@peer/greet" {
		t.Errorf("proxy action = %q", got)
	}
}

// A login, a creation, a recovery and a rename take the account's address and must name this
// kernel; a user of another kernel is refused, not dialled.
func TestLocalHandleRefusesOtherKernels(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	if _, err := k.BindPetname(ctx, testKernelKey(8), "elsewhere", true); err != nil {
		t.Fatal(err)
	}
	if h, err := k.LocalHandle(ctx, "carol@k"); err != nil || h != "carol" {
		t.Errorf("LocalHandle(carol@k) = %q, %v", h, err)
	}
	for _, bad := range []string{"carol", "carol@elsewhere", "carol@k/act", "carol@nowhere"} {
		if _, err := k.LocalHandle(ctx, bad); err == nil {
			t.Errorf("LocalHandle(%q): want an error", bad)
		}
	}
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "carol@elsewhere", Password: "password123"}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("an account created on another kernel's name: want ErrInvalidInput, got %v", err)
	}
}
