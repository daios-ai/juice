// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"strings"
)

// Address is the one name form (D15): `user@kernel` names a user, `user@kernel/name` an action. The
// user segment is a handle or a user id, the kernel segment a petname — this kernel's own name is one,
// bound to its own key — or a raw key; each pair is disjoint by shape, so nothing guesses. Written
// with an id and a key, an address is a principal, meaning the same party on every kernel. A bare
// handle is not a name anywhere: like an email address, the local part means nothing without its host.
type Address struct {
	User   string // the user's handle or id on Kernel
	Kernel string // petname or raw key, as typed; never stored, never signed
	Name   string // action name, possibly with `/`; "" names the user, or an owner's root
}

// ParseAddress reads user@kernel[/name]. The first `/` ends the head; the head is user@kernel, both
// parts non-empty, the user a bare handle or an id. A raw action id is a different production
// (looksLikeID) and is refused here so an id is never mistaken for a path.
func ParseAddress(s string) (Address, error) {
	s = strings.TrimSpace(s)
	head, name, _ := strings.Cut(s, "/")
	handle, kernelSeg, hasKernel := strings.Cut(head, "@")
	if !hasKernel || handle == "" || kernelSeg == "" || (strings.Contains(s, "/") && name == "") || strings.ContainsAny(s, " \t\r\n") {
		return Address{}, ErrInvalidInput.Wrapf("%q is not an address: name the user as handle@kernel, an action as handle@kernel/name", s)
	}
	if !looksLikeID(handle) {
		if err := ValidateHandle(handle); err != nil {
			return Address{}, err
		}
	}
	if ValidateHandle(kernelSeg) != nil && !looksLikeKey(kernelSeg) {
		return Address{}, ErrInvalidInput.Wrapf("%q is not a kernel name or key", kernelSeg)
	}
	return Address{User: handle, Kernel: kernelSeg, Name: name}, nil
}

// errUserNotFound is the one refusal for a user reference that names nobody, so every resolver
// reports a miss in the same words and the reference as it was written.
func errUserNotFound(ref string) error { return ErrNotFound.Wrapf("user %s not found", ref) }

// String renders user@kernel[/name].
func (a Address) String() string {
	if a.Name == "" {
		return a.User + "@" + a.Kernel
	}
	return a.User + "@" + a.Kernel + "/" + a.Name
}

// readUser reads a user of this kernel by the user segment of an address: an id, or a handle. The
// one reader for local input and for a peer's resolve alike, so a principal answers wherever a
// handle does (D15).
func (k *Kernel) readUser(ctx context.Context, seg string) (*Account, error) {
	if looksLikeID(seg) {
		return k.store.ReadUser(ctx, seg)
	}
	return k.store.ReadUserByHandle(ctx, seg)
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

// Written is a principal as a program keeps it (D15): user_id@kernel_key, a kernel alone as its
// key, and empty for nobody. self is this kernel's key, which a party here leaves implicit.
func (p Principal) Written(self string) string {
	key := firstNonEmpty(p.Kernel, self)
	switch {
	case p.UserID != "":
		return p.UserID + "@" + key
	case p.Kernel != "":
		return key
	}
	return ""
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
	a, err := k.localAddress(ctx, ref)
	if err != nil {
		return nil, err
	}
	u, err := k.readUser(ctx, a.User)
	if err != nil || u == nil {
		return nil, errUserNotFound(ref)
	}
	return u, nil
}

// LocalHandle reads an account's own name on this kernel out of the address it is written as:
// `handle@<this kernel>`. Creating, logging into, recovering and renaming an account all take the
// address, and refuse one that names another kernel — an account lives where it was made — or that
// is written with an id, since each of them names or chooses a handle.
func (k *Kernel) LocalHandle(ctx context.Context, addr string) (string, error) {
	a, err := k.localAddress(ctx, addr)
	if err != nil {
		return "", err
	}
	if err := ValidateHandle(a.User); err != nil {
		return "", err
	}
	return a.User, nil
}

// localAddress parses an address that must name a user of this kernel, by handle or by id.
func (k *Kernel) localAddress(ctx context.Context, addr string) (Address, error) {
	a, err := ParseAddress(addr)
	if err != nil {
		return Address{}, err
	}
	if a.Name != "" {
		return Address{}, ErrInvalidInput.Wrapf("%q names an action; an account is handle@kernel", addr)
	}
	kr, err := k.ResolveKernel(ctx, a.Kernel)
	if err != nil {
		return Address{}, err
	}
	if !kr.Local {
		return Address{}, ErrInvalidInput.Wrapf("%s is on another kernel, not on %s", addr, k.OwnName(ctx))
	}
	return a, nil
}

// ResolvePrincipal resolves an address to who a task may be parked for (P8): a user here, or a user on
// a peer — the peer's key and the user's stable id there, resolved over /juice/fed/resolve/1. The
// verified resolve makes the peer known; it names it nothing, since only the operator binds a
// petname (D15).
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
		u, uerr := k.readUser(ctx, a.User)
		if uerr != nil || u == nil {
			return Principal{}, errUserNotFound(ref)
		}
		return User(u.ID), nil
	}
	if k.fedClient == nil {
		return Principal{}, ErrNotFound.Wrap("remote resolution unavailable")
	}
	res, rerr := k.fedClient.ResolveRemoteUser(ctx, kr.Key, a.User)
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
	return Principal{Kernel: kr.Key, UserID: res.UserID, Handle: NormalizeHandle(res.Handle)}, nil
}

// LocalPrincipal answers the open /juice/fed/resolve/1 user question for a peer: a user of this
// kernel who is not suspended, named by the bare handle or id the peer's address carried, to its
// stable id and current handle; ids are addresses, not secrets.
func (k *Kernel) LocalPrincipal(ctx context.Context, ref string) (userID, handle string, err error) {
	u, err := k.readUser(ctx, ref)
	if err != nil || u == nil || u.SuspendedAt != nil {
		return "", "", errUserNotFound(ref)
	}
	return u.ID, u.Handle, nil
}

// ---- Rendering ----

// AddressView is how a reply names a party or an action (D15, D20): the address a person reads and
// types, the principal a program keeps — the same party on every kernel, whatever it is called — and,
// for a kernel this one has given no petname, the nickname it calls itself, shown after its key and
// never resolved.
type AddressView struct {
	Address   string `json:"address"`
	Principal string `json:"principal"`
	Nickname  string `json:"nickname,omitempty"`
}

// String is what a person reads: the address, then the nickname marked as the party's own claim.
func (v AddressView) String() string {
	if v.Nickname == "" {
		return v.Address
	}
	return v.Address + " ~" + v.Nickname
}

// Names renders principals and actions, reading each account and kernel row at most once: a
// transaction list names few distinct parties across many rows. One per request.
type Names struct {
	k        *Kernel
	accounts map[string]*Account
	kernels  map[string]AddressView
	self     string
	own      string
}

// NewNames starts one rendering scope.
func (k *Kernel) NewNames() *Names {
	return &Names{k: k, accounts: map[string]*Account{}, kernels: map[string]AddressView{}}
}

func (n *Names) account(ctx context.Context, id string) *Account {
	if u, ok := n.accounts[id]; ok {
		return u
	}
	u, _ := n.k.store.ReadUser(ctx, id) // nil on error; cached so a bad id isn't re-read
	n.accounts[id] = u
	return u
}

// KeyAddress is a kernel known only by its key: the key is both its address and its principal.
func KeyAddress(key string) AddressView { return AddressView{Address: key, Principal: key} }

// Kernel renders a kernel (D15): its petname, else its key with the nickname it gives itself beside
// it, so the true key is always what an unnamed kernel is shown and typed by.
func (n *Names) Kernel(ctx context.Context, key string) AddressView {
	if v, ok := n.kernels[key]; ok {
		return v
	}
	v := KeyAddress(key)
	if rk, err := n.k.store.ReadKernel(ctx, key); err == nil && rk != nil {
		if rk.Petname != "" {
			v.Address = rk.Petname
		} else if ValidateHandle(rk.Nickname) == nil {
			v.Nickname = rk.Nickname
		}
	}
	n.kernels[key] = v
	return v
}

func (n *Names) local(ctx context.Context) (self, own string) {
	if n.self == "" {
		n.self, n.own = n.k.SelfKey(ctx), n.k.OwnName(ctx)
	}
	return n.self, n.own
}

// Address renders a principal (D20): a user here as handle@<own name>; a user on a peer as their
// handle beneath the peer's name; the peer kernel itself as that name alone. A user always carries
// `@`, a kernel never does, so the two cannot be confused however they are named. Its principal is
// the user's id beneath the kernel's key. An id that names nobody here — a party purged before
// kernels were named by key — renders as itself, so an immutable ledger stays legible.
func (n *Names) Address(ctx context.Context, p Principal) AddressView {
	if !p.Local() {
		v := n.Kernel(ctx, p.Kernel)
		if p.UserID == "" {
			return v
		}
		return v.under(firstNonEmpty(p.Handle, p.UserID), p.UserID, p.Kernel)
	}
	if p.UserID == "" {
		return AddressView{}
	}
	self, own := n.local(ctx)
	v := AddressView{Address: p.UserID, Principal: p.Written(self)}
	if u := n.account(ctx, p.UserID); u != nil {
		v.Address = u.Handle + "@" + own
	}
	return v
}

// Action renders an action: its owner's address and principal, then the name. A proxy's owner is
// the peer's user recorded on the row (D13), beneath the peer's name here.
func (n *Names) Action(ctx context.Context, a *Action) AddressView {
	if a == nil {
		return AddressView{}
	}
	return n.Address(ctx, a.Owner()).Child(a.Name)
}

// under renders a user of the kernel v names, by the handle a record or manifest carries and the id
// that is their principal there.
func (v AddressView) under(handle, userID, key string) AddressView {
	v.Address = handle + "@" + v.Address
	v.Principal = userID + "@" + key
	return v
}

// Child renders a name beneath a party: an action of its owner.
func (v AddressView) Child(name string) AddressView {
	v.Address += "/" + name
	v.Principal += "/" + name
	return v
}

// Address is the single-shot form of Names.Address.
func (k *Kernel) Address(ctx context.Context, p Principal) AddressView {
	return k.NewNames().Address(ctx, p)
}

// ActionAddress is the single-shot form of Names.Action.
func (k *Kernel) ActionAddress(ctx context.Context, a *Action) AddressView {
	return k.NewNames().Action(ctx, a)
}

// KernelAddress is the single-shot form of Names.Kernel.
func (k *Kernel) KernelAddress(ctx context.Context, key string) AddressView {
	return k.NewNames().Kernel(ctx, key)
}

// ActionAddressByID renders an action named by id, falling back to the id when the row is gone.
func (k *Kernel) ActionAddressByID(ctx context.Context, actionID string) AddressView {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil || a == nil {
		return AddressView{Address: actionID, Principal: actionID}
	}
	return k.ActionAddress(ctx, a)
}
