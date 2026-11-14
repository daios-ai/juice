// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

// userPeer is a peer that answers the user resolve, and nothing else (D15).
type userPeer struct {
	kernel.FederationClient
	asked []string
}

func (p *userPeer) ResolveRemoteUser(_ context.Context, _, user string) (*kernel.ResolvedUser, error) {
	p.asked = append(p.asked, user)
	return &kernel.ResolvedUser{UserID: "far-id", Handle: "far"}, nil
}

// sys/resolve is a program's name service (D17): a user's address, by handle or as a principal, here
// or on a peer, in; the address this kernel shows them by and their principal out, the same for
// every form. An action's or a kernel's address is not a user's, and an unknown user is not found.
func TestResolveTurnsAUsersAddressIntoTheirPrincipal(t *testing.T) {
	k, st := newLookupTestKernel(t)
	ctx := context.Background()
	peer := &userPeer{}
	k.SetFederation(peer)
	self := k.SelfKey(ctx)
	alice := seedOwner(t, st, "alice")
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	far := base64.RawURLEncoding.EncodeToString(pub)
	resolve := Resolve().Handler(k)
	run := func(address string) (map[string]any, error) {
		return resolve(ctx, map[string]any{"address": address}, "", "", "", "", "", kernel.Parties{})
	}

	for _, address := range []string{"alice@k", alice.ID + "@k", alice.ID + "@" + self} {
		got, err := run(address)
		if err != nil || got["address"] != "alice@k" || got["principal"] != alice.ID+"@"+self {
			t.Errorf("resolve %s = %v, %v; want alice@k and %s@%s", address, got, err, alice.ID, self)
		}
	}
	for _, address := range []string{"far@" + far, "far-id@" + far} {
		got, err := run(address)
		if err != nil || got["address"] != "far@"+far || got["principal"] != "far-id@"+far {
			t.Errorf("resolve %s = %v, %v; want far@<key> and far-id@<key>", address, got, err)
		}
	}
	if len(peer.asked) != 2 || peer.asked[0] != "far" || peer.asked[1] != "far-id" {
		t.Errorf("the peer was asked %v; want the user as typed, by handle then by id", peer.asked)
	}
	for _, address := range []string{"alice@k/act", "k", self} {
		if _, err := run(address); !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("resolve %s: %v, want ErrInvalidInput", address, err)
		}
	}
	if _, err := run("nobody@k"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("resolve an unknown user: %v, want ErrNotFound", err)
	}
}
