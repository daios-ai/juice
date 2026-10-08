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

// errUserNotFound is the one refusal for a user reference that names nobody, so every resolver
// reports a miss in the same words and the reference as it was written.
func errUserNotFound(ref string) error { return ErrNotFound.Wrapf("user %s not found", ref) }

// String renders handle@kernel[/name].
func (a Address) String() string {
	if a.Name == "" {
		return a.Handle + "@" + a.Kernel
	}
	return a.Handle + "@" + a.Kernel + "/" + a.Name
}

// Principal is who a record names (D15): the kernel the party lives on, empty for this one, and the
// user there, empty for that kernel itself — a user here, a user on a peer, or the peer. Handle is
// what a user on a peer was called when the record was made: display only, exactly as a proxy's
// owner_handle is (P6), so it is never compared. A principal is a name, not an authority: every
// path validates its caller before reading what kind of party it is.
type Principal struct {
	Kernel string
	UserID string
	Handle string
}

// User is a user of this kernel as a principal.
func User(id string) Principal { return Principal{UserID: id} }

// Local reports a party of this kernel; IsKernel a peer kernel itself, with no user of its named.
func (p Principal) Local() bool    { return p.Kernel == "" }
func (p Principal) IsKernel() bool { return p.Kernel != "" && p.UserID == "" }

// Same compares two principals by identity: the kernel and the user id, never the handle.
func (p Principal) Same(q Principal) bool { return p.Kernel == q.Kernel && p.UserID == q.UserID }

// IsUser reports whether p is the user of this kernel with id — which a user of a peer whose id
// happens to be the same never is.
func (p Principal) IsUser(id string) bool { return p.Local() && id != "" && p.UserID == id }

// KernelRef is a resolved kernel segment: the key it names, and whether that is this kernel.
type KernelRef struct {
	Key   string
	Local bool
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
	return KernelRef{Key: key, Local: key == k.SelfKey(ctx)}, nil
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
	if err != nil || u == nil {
		return nil, errUserNotFound(ref)
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

// ResolvePrincipal resolves an address to who a task may be parked for (P8): a user here, or a user on
// a peer — the peer's key and the user's stable id there, resolved over /juice/fed/resolve/1. The
// verified resolve makes the peer known and binds its petname too, best-effort: naming turns on our
// own outbound act, never on a peer's.
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
		if uerr != nil || u == nil {
			return Principal{}, errUserNotFound(ref)
		}
		return User(u.ID), nil
	}
	if k.fedClient == nil {
		return Principal{}, ErrNotFound.Wrap("remote resolution unavailable")
	}
	res, rerr := k.fedClient.ResolveRemoteUser(ctx, kr.Key, a.Handle)
	if rerr != nil {
		return Principal{}, rerr
	}
	// An empty id is not a principal. Accepted, it would address the task to the peer kernel
	// itself — operator scope, decided by a remote reply — and strand the user it was meant for.
	if res == nil || res.UserID == "" {
		return Principal{}, ErrInvalidInput.Wrapf("peer resolved %q to no user id", ref)
	}
	// Where the peer is paid, proved by its own rail key: a transfer to this user pays there (P11).
	if _, verr := k.ObservePeerVault(ctx, kr.Key, res.BlockchainAddress, res.BlockchainProof); verr != nil {
		return Principal{}, verr
	}
	if err := k.knowKernel(ctx, kr.Key); err != nil {
		return Principal{}, err
	}
	if _, berr := k.BindPetname(ctx, kr.Key, "", false); berr != nil {
		k.log.With(ctx).Warn("kernel.petname.bind_failed", "public_key", kr.Key, "error", berr.Error())
	}
	return Principal{Kernel: kr.Key, UserID: res.UserID, Handle: NormalizeHandle(res.Handle)}, nil
}

// LocalPrincipal answers the open /juice/fed/resolve/1 user question for a peer: a user of this
// kernel who is not suspended, named by the bare handle the peer's address carried, to its stable id
// and current handle; ids are addresses, not secrets.
func (k *Kernel) LocalPrincipal(ctx context.Context, ref string) (userID, handle string, err error) {
	u, err := k.store.ReadUserByHandle(ctx, ref)
	if err != nil || u == nil || u.SuspendedAt != nil {
		return "", "", errUserNotFound(ref)
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
// itself as that name alone. A user always carries `@`, a kernel never does, so the two cannot be
// confused however they are named. An id that names nobody here — a party purged before kernels
// were named by key — renders as itself, so an immutable ledger stays legible.
func (n *Names) Address(ctx context.Context, p Principal) string {
	if !p.Local() {
		host := n.kernel(ctx, p.Kernel)
		if who := firstNonEmpty(p.Handle, p.UserID); who != "" {
			return who + "@" + host
		}
		return host
	}
	if p.UserID == "" {
		return ""
	}
	if u := n.account(ctx, p.UserID); u != nil {
		return u.Handle + "@" + n.ownName(ctx)
	}
	return p.UserID
}

// Action renders an action's address: its owner's, then the name. A proxy's owner is the peer's
// user recorded on the row (D13), beneath the peer's name here.
func (n *Names) Action(ctx context.Context, a *Action) string {
	if a == nil {
		return ""
	}
	return n.Address(ctx, a.Owner()) + "/" + a.Name
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
