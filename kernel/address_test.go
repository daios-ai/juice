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

// An address is user@kernel[/name], the user a handle or an id and the kernel a petname or a key, so a
// principal parses as an address; anything else — a bare handle, owner/name, a sigil, an empty half,
// whitespace — is refused (D15).
func TestParseAddress(t *testing.T) {
	id, key := "0f3c2a1e-5b7d-4e8a-9c0b-1d2e3f4a5b6c", testKernelKey(2)
	for _, c := range []struct{ in, handle, kernel, name string }{
		{id + "@" + key, id, key, ""},
		{id + "@k/brief", id, "k", "brief"},
		{"tom@" + key + "/brief", "tom", key, "brief"},
		{"tom@k", "tom", "k", ""},
		{"tom@k/brief", "tom", "k", "brief"},
		{"tom@k/brief/eu", "tom", "k", "brief/eu"},
		{"tom@k/mail@home", "tom", "k", "mail@home"}, // only the head qualifies a kernel
		{" tom@k/brief ", "tom", "k", "brief"},
	} {
		a, err := kernel.ParseAddress(c.in)
		if err != nil || a.User != c.handle || a.Kernel != c.kernel || a.Name != c.name {
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
// key, no peer can be given it, a peer calling itself the same is shown by its key with that nickname,
// the kernel's own commands refuse it as a peer, and the stale-discovery sweep never evicts it.
func TestOwnNameIsAPetnameOnTheOwnKey(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	sys := setupSys(t, k, st)
	self := k.SelfKey(ctx)

	for _, seg := range []string{kernel.TestOwnName, self} {
		kr, err := k.ResolveKernel(ctx, seg)
		if err != nil || !kr.Local || kr.Key != self {
			t.Fatalf("ResolveKernel(%q) = %+v, %v; want this kernel", seg, kr, err)
		}
	}
	if _, err := k.BindPetname(ctx, testKernelKey(3), kernel.TestOwnName); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("a peer took this kernel's own name: %v", err)
	}
	if err := k.ObserveKernel(ctx, testKernelKey(4), kernel.TestOwnName, ""); err != nil {
		t.Fatal(err)
	}
	if v := k.KernelAddress(ctx, testKernelKey(4)); v.Address != testKernelKey(4) || v.Nickname != kernel.TestOwnName {
		t.Errorf("a peer calling itself by our name is shown as %+v; want its key, with the nickname beside it", v)
	}
	if _, err := k.RenameKernel(ctx, sys.ID, self, "other"); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("renaming this kernel as a peer: want ErrInvalidInput, got %v", err)
	}
	if _, err := knownPeer(k, ctx, self); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("a kernel as its own peer: want ErrInvalidInput, got %v", err)
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
	if _, err := k.BindPetname(ctx, testKernelKey(5), "taken"); err != nil {
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
	if _, err := k.BindPetname(ctx, testKernelKey(9), "peer"); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]bool{
		"alice@k/x":                         false,
		"alice@" + k.SelfKey(ctx) + "/x":    false,
		"alice@peer/x":                      true,
		"alice@nowhere/x":                   true,
		"alice@" + testKernelKey(10) + "/x": true,
		"alice/x":                           false,
	} {
		if got := k.IsRemoteRef(ctx, ref); got != want {
			t.Errorf("IsRemoteRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

// Every party renders as an address for a person and a principal for a program (D15, D20): a user
// here beneath the own name, a peer's user beneath the peer's petname, the peer kernel itself as its
// name alone, a peer without a petname as its full key with its own nickname beside it, and an id
// naming nobody here as itself. The principal is always the user's id beneath the kernel's key. An
// action's is its owner's plus the name, a proxy's the remote owner's beneath the peer.
func TestNamesRenderPrincipalsAndActions(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	self := k.SelfKey(ctx)
	alice := setupUser(t, st, "alice", 0)
	named, err := knownPeer(k, ctx, testKernelKey(6))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.BindPetname(ctx, testKernelKey(6), "peer"); err != nil {
		t.Fatal(err)
	}
	if err := k.ObserveKernel(ctx, testKernelKey(6), "ignored", ""); err != nil {
		t.Fatal(err)
	}
	unbound, err := knownPeer(k, ctx, testKernelKey(7))
	if err != nil {
		t.Fatal(err)
	}
	nicked := testKernelKey(8)
	if err := k.ObserveKernel(ctx, nicked, "acme", ""); err != nil {
		t.Fatal(err)
	}
	names := k.NewNames()
	v := func(address, principal, nickname string) kernel.AddressView {
		return kernel.AddressView{Address: address, Principal: principal, Nickname: nickname}
	}
	for _, c := range []struct {
		p    kernel.Principal
		want kernel.AddressView
	}{
		{kernel.User(alice.ID), v("alice@k", alice.ID+"@"+self, "")},
		{kernel.Principal{Kernel: named}, v("peer", named, "")},
		{kernel.Principal{Kernel: named, UserID: "u-1", Handle: "bob"}, v("bob@peer", "u-1@"+named, "")},
		{kernel.Principal{Kernel: named, UserID: "u-1"}, v("u-1@peer", "u-1@"+named, "")},
		{kernel.Principal{Kernel: named, UserID: alice.ID}, v(alice.ID+"@peer", alice.ID+"@"+named, "")},
		{kernel.Principal{Kernel: unbound}, v(unbound, unbound, "")},
		{kernel.Principal{Kernel: nicked, UserID: "u-2", Handle: "carol"}, v("carol@"+nicked, "u-2@"+nicked, "acme")},
		{kernel.Principal{Kernel: nicked}, v(nicked, nicked, "acme")},
		{kernel.User("gone"), v("gone", "gone@"+self, "")},
		{kernel.Principal{}, v("", "", "")},
	} {
		if got := names.Address(ctx, c.p); got != c.want {
			t.Errorf("Address(%+v) = %+v, want %+v", c.p, got, c.want)
		}
	}
	if got := names.Address(ctx, kernel.Principal{Kernel: nicked}).String(); got != nicked+" ~acme" {
		t.Errorf("a kernel without a petname reads %q; want its key, then ~acme", got)
	}
	local := &kernel.Action{OwnerUserID: alice.ID, Name: "greet"}
	proxy := &kernel.Action{OwnerKernel: named, OwnerUserID: "u-1", OwnerHandle: "bob", Name: "greet", Kind: kernel.KindRemoteProxy}
	if got := names.Action(ctx, local); got != v("alice@k/greet", alice.ID+"@"+self+"/greet", "") {
		t.Errorf("local action = %+v", got)
	}
	if got := names.Action(ctx, proxy); got != v("bob@peer/greet", "u-1@"+named+"/greet", "") {
		t.Errorf("proxy action = %+v", got)
	}
}

// A login, a creation, a recovery and a rename take the account's address and must name this
// kernel; a user of another kernel is refused, not dialled.
func TestLocalHandleRefusesOtherKernels(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	if _, err := k.BindPetname(ctx, testKernelKey(8), "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if h, err := k.LocalHandle(ctx, "carol@k"); err != nil || h != "carol" {
		t.Errorf("LocalHandle(carol@k) = %q, %v", h, err)
	}
	// A principal names an existing account, never the handle these operations take or choose.
	for _, bad := range []string{"carol", "carol@elsewhere", "carol@k/act", "carol@nowhere", "0f3c2a1e-5b7d-4e8a-9c0b-1d2e3f4a5b6c@k"} {
		if _, err := k.LocalHandle(ctx, bad); err == nil {
			t.Errorf("LocalHandle(%q): want an error", bad)
		}
	}
	if _, err := k.CreateUser(ctx, kernel.CreateUserRequest{Handle: "carol@elsewhere", Password: "password123"}); !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("an account created on another kernel's name: want ErrInvalidInput, got %v", err)
	}
}
