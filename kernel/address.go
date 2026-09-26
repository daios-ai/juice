// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"strings"
)

// Address is the one name form (D15): `handle@kernel` names a user, `handle@kernel/name` an action.
// The kernel segment is a petname — this kernel's own name is one, bound to its own key — or a raw
// key; the two are disjoint by shape, so nothing guesses. A bare handle is not a name anywhere: like
// an email address, the local part means nothing without its host.
type Address struct {
	Handle string // the user's handle on Kernel
	Kernel string // petname or raw key, as typed; never stored, never signed
	Name   string // action name, possibly with `/`; "" names the user, or an owner's root
}

// ParseAddress reads handle@kernel[/name]. The first `/` ends the head; the head is handle@kernel,
// both parts non-empty, the handle bare. A raw action id is a different production (looksLikeID)
// and is refused here so an id is never mistaken for a path.
func ParseAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	head, name, _ := strings.Cut(s, "/")
	handle, kernelSeg, hasKernel := strings.Cut(head, "@")
	if !hasKernel || handle == "" || kernelSeg == "" || (strings.Contains(s, "/") && name == "") || strings.ContainsAny(s, " \t\r\n") {
		return Address{}, ErrInvalidInput.Wrapf("%q is not an address: name the user as handle@kernel, an action as handle@kernel/name", s)
	}
	if err := ValidateHandle(handle); err != nil {
		return Address{}, err
	}
	if ValidateHandle(kernelSeg) != nil && !looksLikeKey(kernelSeg) {
		return Address{}, ErrInvalidInput.Wrapf("%q is not a kernel name or key", kernelSeg)
	}
	return Address{Handle: handle, Kernel: kernelSeg, Name: name}, nil
}

// String renders handle@kernel[/name].
func (a Address) String() string {
	if a.Name == "" {
		return a.Handle + "@" + a.Kernel
	}
	return a.Handle + "@" + a.Kernel + "/" + a.Name
}

// Principal is who a record names: the local account that funds and routes (a user here, or a peer
// kernel's billing account) and, when that account stands for a user on the peer, that user's
// stable id there and the handle they went by at the time. The id is the identity; the handle
// only displays it, exactly as a proxy's owner_handle does (P6).
type Principal struct {
	AccountID string
	RemoteID  string
	Handle    string
}

// KernelRef is a resolved kernel segment: the key it names, whether that is this kernel, and the
// peer's billing account when one exists (nil until money has been involved, and nil for self).
type KernelRef struct {
	Key     string
	Local   bool
	Account *Account
}

// ResolveKernel is the one place a kernel segment becomes a key (D15): a bound petname — this
// kernel's own name among them — else a raw key. A self-asserted nickname never resolves, so no
// kernel can capture a name by gossiping it first.
func (k *Kernel) ResolveKernel(ctx context.Context, seg string) (KernelRef, error) {
	seg = strings.TrimSpace(seg)
	var key string
	if rk, err := k.store.ReadKernelByPetname(ctx, seg); err == nil && rk != nil {
		key = rk.PublicKey
	} else if looksLikeKey(seg) {
		key = seg
	} else {
		return KernelRef{}, ErrNotFound.Wrapf("kernel %q is not a known name or key", seg)
	}
	if key == k.selfKey(ctx) {
		return KernelRef{Key: key, Local: true}, nil
	}
	acct, _ := k.store.ReadAccountByKernelKey(ctx, key)
	return KernelRef{Key: key, Account: acct}, nil
}

// IsRemoteRef reports whether a reference is one this kernel does not answer itself: it names a
// peer, whose resolution may dial, or a kernel nobody here knows, which resolves to nothing. Only a
// reference on this kernel's own name or key is not remote; a malformed one is not remote either,
// since the resolver refuses it like any other bad input.
func (k *Kernel) IsRemoteRef(ctx context.Context, ref string) bool {
	a, err := ParseAddress(ref)
	if err != nil {
		return false
	}
	kr, err := k.ResolveKernel(ctx, a.Kernel)
	return err != nil || !kr.Local
}

// ResolveLocalPrincipal resolves an address that must name a live user of this kernel: a
// transfer's recipient, an admin target. A remote address is refused here rather than dialled.
func (k *Kernel) ResolveLocalPrincipal(ctx context.Context, ref string) (*Account, error) {
	handle, err := k.LocalHandle(ctx, ref)
	if err != nil {
		return nil, err
	}
	u, err := k.store.ReadUserByHandle(ctx, handle)
	if err != nil || u == nil || !u.IsLiveUser() {
		return nil, ErrNotFound.Wrapf("user %s not found", ref)
	}
	return u, nil
}

// LocalHandle reads an account's own name on this kernel out of the address it is written as:
// `handle@<this kernel>`. Creating, logging into, recovering and renaming an account all take the
// address, and refuse one that names another kernel — an account lives where it was made.
func (k *Kernel) LocalHandle(ctx context.Context, addr string) (string, error) {
	a, err := ParseAddress(addr)
	if err != nil {
		return "", err
	}
	if a.Name != "" {
		return "", ErrInvalidInput.Wrapf("%q names an action; an account is handle@kernel", addr)
	}
	kr, err := k.ResolveKernel(ctx, a.Kernel)
	if err != nil {
		return "", err
	}
	if !kr.Local {
		return "", ErrInvalidInput.Wrapf("%s is on another kernel, not on %s", addr, k.OwnName(ctx))
	}
	return a.Handle, nil
}

// ResolvePrincipal resolves an address to who a step may be parked for (P8): a live user here, or a
// user on a peer — its stable id resolved over /juice/fed/resolve/1 and its billing account here,
// which is the peer's, opened on this kernel's own act. The verified resolve binds the peer's
// petname too, best-effort: naming turns on our own outbound act, never on a peer's.
func (k *Kernel) ResolvePrincipal(ctx context.Context, ref string) (Principal, error) {
	a, err := ParseAddress(ref)
	if err != nil {
		return Principal{}, err
	}
	if a.Name != "" {
		return Principal{}, ErrInvalidInput.Wrapf("%q names an action, not a user", ref)
	}
	kr, err := k.ResolveKernel(ctx, a.Kernel)
	if err != nil {
		return Principal{}, err
	}
	if kr.Local {
		u, uerr := k.store.ReadUserByHandle(ctx, a.Handle)
		if uerr != nil || u == nil || !u.IsLive() {
			// A tombstone resolves but can never complete: the step would park its price forever.
			return Principal{}, ErrNotFound.Wrapf("user %s not found", ref)
		}
		return Principal{AccountID: u.ID}, nil
	}
	if k.fedClient == nil {
		return Principal{}, ErrNotFound.Wrap("remote resolution unavailable")
	}
	remoteID, remoteHandle, rerr := k.fedClient.ResolveRemoteUser(ctx, kr.Key, a.Handle)
	if rerr != nil {
		return Principal{}, rerr
	}
	// An empty id is not a principal. Accepted, it would address the step to the peer kernel
	// itself — operator scope, decided by a remote reply — and strand the user it was meant for.
	if remoteID == "" {
		return Principal{}, ErrInvalidInput.Wrapf("peer resolved %q to no user id", ref)
	}
	if _, berr := k.BindPetname(ctx, kr.Key, "", false); berr != nil {
		k.log.With(ctx).Warn("kernel.petname.bind_failed", "public_key", kr.Key, "error", berr.Error())
	}
	mount := kr.Account
	if mount == nil {
		if mount, err = k.EnsureKernelAccount(ctx, kr.Key); err != nil {
			return Principal{}, err
		}
	}
	return Principal{AccountID: mount.ID, RemoteID: remoteID, Handle: NormalizeHandle(remoteHandle)}, nil
}

// LocalPrincipal answers the open /juice/fed/resolve/1 user question for a peer: a live user of
// this kernel, named by the bare handle the peer's address carried, to its stable id and current
// handle. Only a live account with a handle resolves; ids are addresses, not secrets.
func (k *Kernel) LocalPrincipal(ctx context.Context, ref string) (userID, handle string, err error) {
	u, err := k.resolveUser(ctx, ref)
	if err != nil || u == nil || u.SuspendedAt != nil || u.Handle == "" {
		return "", "", ErrNotFound.Wrapf("user %s not found", ref)
	}
	return u.ID, u.Handle, nil
}

// ---- Rendering ----

// Names renders principals and actions as addresses, reading each account and kernel row at most
// once: a transaction list names few distinct parties across many rows. One per request.
type Names struct {
	k        *Kernel
	accounts map[string]*Account
	kernels  map[string]string
	own      string
}

// NewNames starts one rendering scope.
func (k *Kernel) NewNames() *Names {
	return &Names{k: k, accounts: map[string]*Account{}, kernels: map[string]string{}}
}

func (n *Names) account(ctx context.Context, id string) *Account {
	if u, ok := n.accounts[id]; ok {
		return u
	}
	u, _ := n.k.store.ReadUser(ctx, id) // nil on error; cached so a bad id isn't re-read
	n.accounts[id] = u
	return u
}

func (n *Names) kernel(ctx context.Context, key string) string {
	if name, ok := n.kernels[key]; ok {
		return name
	}
	name := n.k.KernelName(ctx, key)
	n.kernels[key] = name
	return name
}

func (n *Names) ownName(ctx context.Context) string {
	if n.own == "" {
		n.own = n.k.OwnName(ctx)
	}
	return n.own
}

// Address renders a principal (D20): a user here as handle@<own name>; a user on a peer as their
// handle beneath the peer's name — its petname, or its key while it has none; the peer kernel
// itself, when no user is known, as that name alone. A user always carries `@`, a kernel never
// does, so the two cannot be confused however they are named. Only a purged tombstone falls back
// to the raw id, so an immutable ledger stays legible.
func (n *Names) Address(ctx context.Context, p Principal) string {
	if p.AccountID == "" {
		return ""
	}
	u := n.account(ctx, p.AccountID)
	if u == nil {
		return p.AccountID
	}
	if u.Handle != "" {
		return u.Handle + "@" + n.ownName(ctx)
	}
	if u.KernelPublicKey != "" {
		host := n.kernel(ctx, u.KernelPublicKey)
		if who := firstNonEmpty(p.Handle, p.RemoteID); who != "" {
			return who + "@" + host
		}
		return host
	}
	return p.AccountID
}

// Action renders an action's address: its owner's, then the name. A proxy's owner is the peer's
// user recorded on the row (D13), beneath the peer's name here.
func (n *Names) Action(ctx context.Context, a *Action) string {
	if a == nil {
		return ""
	}
	if a.Kind == KindRemoteProxy {
		owner, rest := SplitProxyName(a.Name)
		return n.Address(ctx, Principal{AccountID: a.OwnerUserID, RemoteID: a.RemoteOwnerID, Handle: owner}) + "/" + rest
	}
	return n.Address(ctx, Principal{AccountID: a.OwnerUserID}) + "/" + a.Name
}

// Address is the single-shot form of Names.Address.
func (k *Kernel) Address(ctx context.Context, p Principal) string {
	return k.NewNames().Address(ctx, p)
}

// ActionAddress is the single-shot form of Names.Action.
func (k *Kernel) ActionAddress(ctx context.Context, a *Action) string {
	return k.NewNames().Action(ctx, a)
}

// ActionAddressByID renders an action named by id, falling back to the id when the row is gone.
func (k *Kernel) ActionAddressByID(ctx context.Context, actionID string) string {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil || a == nil {
		return actionID
	}
	return k.ActionAddress(ctx, a)
}

// ---- The proxy row's stored name ----

// A proxy row is stored under the peer's billing account with the remote owner's handle folded into
// its name, `remoteowner/rest`, which is what keeps (owner, name) unique across a peer's many
// owners. The fold is a storage encoding and these two functions are its only readers and writer.

// SplitProxyName decodes a stored proxy name into the remote owner's handle and the action's name.
func SplitProxyName(stored string) (owner, name string) {
	owner, name, _ = strings.Cut(stored, "/")
	return owner, name
}

// JoinProxyName encodes the pair the way the row stores it.
func JoinProxyName(owner, name string) string { return owner + "/" + name }
