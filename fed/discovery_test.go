package fed

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Discovery-by-key with no dedicated seed: three transports where one (R) is the bootstrap.
// Every transport now runs a DHT server + relay, so a plain kernel is the meeting point. B knows
// only R, yet resolves A by public key through R's DHT and round-trips gossip — the loopback
// analogue of a home kernel being found by key with no dialable address and no separate seed.
func TestDiscoveryByKeyViaBootstrapKernel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DHT discovery test in short mode")
	}
	// R is the bootstrap/relay node (an ordinary transport).
	r := newTestTransport(t, &fakeHandlers{}, nil)
	boot := r.ListenAddrs()

	// A serves gossip; B knows only R and must find A by key.
	a := newTestTransport(t, &fakeHandlers{gossip: json.RawMessage(`{"public_key":"a"}`)}, boot)
	b := newTestTransport(t, &fakeHandlers{}, boot)

	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		g, err := b.Gossip(ctx, a.PublicKey())
		cancel()
		if err == nil && string(g) == `{"public_key":"a"}` {
			return // discovered by key through R's DHT and round-tripped
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("B never discovered A by key via the bootstrap kernel: %v", lastErr)
}

// A publicly-reachable transport offers the circuit-relay service (folded into every host); this
// just asserts the relay is wired without error on a normal transport.
func TestTransportOffersRelay(t *testing.T) {
	tr := newTestTransport(t, &fakeHandlers{}, nil)
	if tr.relay == nil {
		t.Error("expected the transport to run a circuit-relay service")
	}
}
