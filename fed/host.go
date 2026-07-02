package fed

import (
	"context"
	"fmt"
	"sync"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/multiformats/go-multiaddr"
)

// Transport is the running federation carrier: a libp2p host plus a Kademlia DHT for
// resolve-by-key, with the five §13 protocols registered. It implements the outbound client
// methods (Call/Friend/Manifests/Gossip/Inspect) and serves inbound streams via Config.Handlers.
type Transport struct {
	host      host.Host
	dht       *dht.IpfsDHT
	cfg       Config
	bootstrap []peer.AddrInfo

	mu     sync.Mutex
	closed bool
}

// New builds and starts a transport host from cfg. It listens, connects to the bootstrap
// peers, starts the DHT, and registers the inbound protocol handlers. Call Close to stop.
func New(ctx context.Context, cfg Config) (*Transport, error) {
	hostKey, err := deriveHostKey(cfg.SigningKey)
	if err != nil {
		return nil, err
	}

	listen := cfg.ListenAddrs
	if len(listen) == 0 {
		// Default: an OS-assigned TCP and QUIC port on all interfaces.
		listen = []string{
			"/ip4/0.0.0.0/tcp/0",
			"/ip4/0.0.0.0/udp/0/quic-v1",
		}
	}

	bootstrap, err := parseAddrInfos(cfg.BootstrapPeers)
	if err != nil {
		return nil, err
	}

	opts := []libp2p.Option{
		libp2p.Identity(hostKey),
		libp2p.ListenAddrStrings(listen...),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.NATPortMap(),
	}
	if len(bootstrap) > 0 {
		opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(bootstrap))
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("fed: build host: %w", err)
	}

	dhtMode := dht.ModeAuto
	dhtOpts := []dht.Option{dht.Mode(dhtMode), dht.BootstrapPeers(bootstrap...)}
	if cfg.AllowPrivateAddrs {
		// Loopback flows run the whole network on 127.0.0.1; permit private addresses in the
		// routing table and query results, which the DHT filters out by default.
		dhtOpts = append(dhtOpts,
			dht.QueryFilter(func(_ interface{}, ai peer.AddrInfo) bool { return true }),
			dht.RoutingTableFilter(func(_ interface{}, p peer.ID) bool { return true }),
		)
	}
	kdht, err := dht.New(ctx, h, dhtOpts...)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("fed: build dht: %w", err)
	}
	if err := kdht.Bootstrap(ctx); err != nil {
		_ = kdht.Close()
		_ = h.Close()
		return nil, fmt.Errorf("fed: bootstrap dht: %w", err)
	}

	t := &Transport{host: h, dht: kdht, cfg: cfg, bootstrap: bootstrap}

	// Connect to bootstrap peers so discovery and relay reservations can proceed.
	for _, ai := range bootstrap {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = h.Connect(cctx, ai)
		cancel()
	}

	t.registerHandlers()
	return t, nil
}

// PublicKey returns this host's base64url Ed25519 public key — its §13 identity.
func (t *Transport) PublicKey() string {
	k, _ := KeyFromPeerID(t.host.ID())
	return k
}

// ListenAddrs returns the multiaddrs (including /p2p/<id>) the host is reachable on. Used
// for logging at server.ready and for the flow harness to scrape a seed's address.
func (t *Transport) ListenAddrs() []string {
	var out []string
	pid := t.host.ID()
	for _, a := range t.host.Addrs() {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, pid))
	}
	return out
}

// Close stops the DHT and host.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	_ = t.dht.Close()
	return t.host.Close()
}

// resolve maps a base64url public key to a connected peer, using the peerstore first and the
// DHT otherwise, then ensures a connection (direct, hole-punched, or relayed) is open.
func (t *Transport) resolve(ctx context.Context, peerKey string) (peer.ID, error) {
	pid, err := PeerIDFromKey(peerKey)
	if err != nil {
		return "", err
	}
	if t.host.Network().Connectedness(pid) == network.Connected {
		return pid, nil
	}
	// Known addresses?
	if addrs := t.host.Peerstore().Addrs(pid); len(addrs) > 0 {
		if err := t.host.Connect(ctx, peer.AddrInfo{ID: pid, Addrs: addrs}); err == nil {
			return pid, nil
		}
	}
	// Resolve via the DHT. On a freshly-formed network the routing table may not yet carry the
	// target, so retry a few times while the DHT settles (bounded by the caller's context).
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		ai, err := t.dht.FindPeer(ctx, pid)
		if err == nil && len(ai.Addrs) > 0 {
			t.host.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.TempAddrTTL)
			if cerr := t.host.Connect(ctx, ai); cerr == nil {
				return pid, nil
			} else {
				lastErr = cerr
			}
		} else if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("fed: cannot resolve peer %s: %w", pid, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("not found")
	}
	return "", fmt.Errorf("fed: cannot resolve peer %s: %w", pid, lastErr)
}

func parseAddrInfos(addrs []string) ([]peer.AddrInfo, error) {
	var out []peer.AddrInfo
	for _, s := range addrs {
		if s == "" {
			continue
		}
		ma, err := multiaddr.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("fed: invalid bootstrap multiaddr %q: %w", s, err)
		}
		ai, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			return nil, fmt.Errorf("fed: bootstrap multiaddr %q missing /p2p/<id>: %w", s, err)
		}
		out = append(out, *ai)
	}
	return out, nil
}
