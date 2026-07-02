package fed

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

// Seed is a bootstrap + relay helper node: a DHT server that helps kernels find each other by
// key, and a circuit-relay v2 service that carries traffic for peers behind strict NAT. It runs
// no kernel and stores no Juice data — it only passes encrypted bytes and holds routing state.
// The same binary runs locally in the flow harness and as a public helper node in production.
type Seed struct {
	host  host.Host
	dht   *dht.IpfsDHT
	relay *relayv2.Relay
}

// NewSeed starts a seed node. A nil/empty key generates an ephemeral identity; passing a fixed
// ed25519 key gives the seed a stable peer ID (so its multiaddr can be published as a bootstrap
// entry). listenAddrs empty means an OS-assigned TCP+QUIC port on all interfaces.
func NewSeed(ctx context.Context, key ed25519.PrivateKey, listenAddrs []string, allowPrivate bool) (*Seed, error) {
	listen := listenAddrs
	if len(listen) == 0 {
		listen = []string{"/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"}
	}
	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(listen...),
		libp2p.EnableNATService(),
	}
	if len(key) == ed25519.PrivateKeySize {
		hk, err := deriveHostKey(key)
		if err != nil {
			return nil, err
		}
		opts = append(opts, libp2p.Identity(hk))
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("fed: seed host: %w", err)
	}
	dhtOpts := []dht.Option{dht.Mode(dht.ModeServer)}
	if allowPrivate {
		dhtOpts = append(dhtOpts,
			dht.QueryFilter(func(_ interface{}, ai peer.AddrInfo) bool { return true }),
			dht.RoutingTableFilter(func(_ interface{}, p peer.ID) bool { return true }),
		)
	}
	kdht, err := dht.New(ctx, h, dhtOpts...)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("fed: seed dht: %w", err)
	}
	if err := kdht.Bootstrap(ctx); err != nil {
		_ = kdht.Close()
		_ = h.Close()
		return nil, err
	}
	r, err := relayv2.New(h)
	if err != nil {
		_ = kdht.Close()
		_ = h.Close()
		return nil, fmt.Errorf("fed: seed relay: %w", err)
	}
	return &Seed{host: h, dht: kdht, relay: r}, nil
}

// Addrs returns the seed's full multiaddrs (including /p2p/<id>) — its bootstrap entries.
func (s *Seed) Addrs() []string {
	var out []string
	for _, a := range s.host.Addrs() {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, s.host.ID()))
	}
	return out
}

// Close stops the relay, DHT, and host.
func (s *Seed) Close() error {
	_ = s.relay.Close()
	_ = s.dht.Close()
	return s.host.Close()
}

var _ = time.Second
