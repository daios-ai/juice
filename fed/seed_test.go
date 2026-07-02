package fed

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

func TestSeedHasStableAddrs(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	seed, err := NewSeed(context.Background(), priv, []string{"/ip4/127.0.0.1/tcp/0"}, true)
	if err != nil {
		t.Fatalf("NewSeed: %v", err)
	}
	defer seed.Close()

	addrs := seed.Addrs()
	if len(addrs) == 0 {
		t.Fatal("seed reported no addrs")
	}
	for _, a := range addrs {
		if !contains(a, "/p2p/") {
			t.Errorf("seed addr missing /p2p/: %s", a)
		}
	}
}

// Discovery-by-key through a seed: two transports that know only the seed resolve each other by
// public key via the seed's DHT — no direct address is configured between them. This is the
// loopback analogue of a home kernel being found by key with no dialable address.
func TestSeedDiscoveryByKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DHT discovery test in short mode")
	}
	_, seedKey, _ := ed25519.GenerateKey(rand.Reader)
	seed, err := NewSeed(context.Background(), seedKey, []string{"/ip4/127.0.0.1/tcp/0"}, true)
	if err != nil {
		t.Fatalf("NewSeed: %v", err)
	}
	defer seed.Close()
	boot := seed.Addrs()

	// A serves gossip; B knows only the seed and must find A by key.
	a := newTestTransport(t, &fakeHandlers{gossip: json.RawMessage(`{"public_key":"a"}`)}, boot)
	b := newTestTransport(t, &fakeHandlers{}, boot)

	// Give the DHT a moment to index A's provider record on the seed.
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		g, err := b.Gossip(ctx, a.PublicKey())
		cancel()
		if err == nil && string(g) == `{"public_key":"a"}` {
			return // discovered by key and round-tripped
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("B never discovered A by key via the seed: %v", lastErr)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
