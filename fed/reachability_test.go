package fed

import (
	"context"
	"testing"
	"time"
)

// A direct loopback connection classifies as "direct".
func TestProbeDirect(t *testing.T) {
	a := newTestTransport(t, &fakeHandlers{}, nil)
	b := newTestTransport(t, &fakeHandlers{}, a.ListenAddrs())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := b.Probe(ctx, a.PublicKey())
	if r.Path != "direct" {
		t.Errorf("path: got %q, want direct", r.Path)
	}
	if r.Error != "" {
		t.Errorf("unexpected error: %s", r.Error)
	}
	if len(r.Protocols) == 0 {
		t.Error("expected the peer to advertise protocols")
	}
}

// An unresolvable key classifies as "unreachable" with an error, and does not hang.
func TestProbeUnreachable(t *testing.T) {
	a := newTestTransport(t, &fakeHandlers{}, nil)
	// A syntactically valid but unroutable key.
	unknown := a.PublicKey()[:len(a.PublicKey())-2] + "ZZ"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	r := a.Probe(ctx, unknown)
	if time.Since(start) > 8*time.Second {
		t.Error("probe took too long for an unreachable peer")
	}
	if r.Path != "unreachable" {
		t.Errorf("path: got %q, want unreachable", r.Path)
	}
}

func TestIsRelayAddr(t *testing.T) {
	if !isRelayAddr("/ip4/1.2.3.4/tcp/1/p2p/QmSeed/p2p-circuit/p2p/QmTarget") {
		t.Error("expected relay addr to be detected")
	}
	if isRelayAddr("/ip4/1.2.3.4/tcp/1/p2p/QmDirect") {
		t.Error("direct addr misclassified as relay")
	}
}
