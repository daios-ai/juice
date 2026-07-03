package fed

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/multiformats/go-multiaddr"
)

// StdPort is the standard Juice federation port. A publicly-reachable kernel that binds it has a
// stable, well-known address others can bootstrap to. Below the Linux ephemeral range (32768+),
// uncommon, and echoes Ethereum's 30303. If it is already taken, the transport falls back to an
// OS-assigned port (kernels are found by key via the DHT, so only a public bootstrap node needs
// the fixed one) and warns.
const StdPort = 31313

// Transport is the running federation carrier: a libp2p host plus a Kademlia DHT for
// resolve-by-key, with the five §13 protocols registered. It implements the outbound client
// methods (Call/Friend/Manifests/Gossip/Inspect) and serves inbound streams via Config.Handlers.
type Transport struct {
	host      host.Host
	dht       *dht.IpfsDHT
	relay     *relayv2.Relay
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

	bootstrap, err := parseAddrInfos(cfg.BootstrapPeers)
	if err != nil {
		return nil, err
	}

	baseOpts := []libp2p.Option{
		libp2p.Identity(hostKey),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.NATPortMap(),
	}
	if len(bootstrap) > 0 {
		baseOpts = append(baseOpts, libp2p.EnableAutoRelayWithStaticRelays(bootstrap))
	}

	build := func(listen []string) (host.Host, error) {
		return libp2p.New(append(baseOpts, libp2p.ListenAddrStrings(listen...))...)
	}

	ephemeral := []string{"/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"}

	// A caller-supplied ListenAddrs is used verbatim. In loopback/test mode (AllowPrivateAddrs)
	// use OS-assigned ports so many kernels can share one host without colliding on the standard
	// port. Otherwise bind the standard port (a public node needs a stable address); if it is
	// taken, fall back to OS-assigned ports (found-by-key doesn't need a fixed port) and warn.
	var h host.Host
	switch {
	case len(cfg.ListenAddrs) > 0:
		h, err = build(cfg.ListenAddrs)
	case cfg.AllowPrivateAddrs:
		h, err = build(ephemeral)
	default:
		std := []string{
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", StdPort),
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", StdPort),
		}
		h, err = build(std)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fed: standard port %d unavailable (%v); using an OS-assigned port instead — set a fixed listen address on a public bootstrap node\n", StdPort, err)
			h, err = build(ephemeral)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fed: build host: %w", err)
	}

	// ModeAuto in production: a publicly-reachable node (a bootstrap host) becomes a DHT server
	// and holds the routing table; NAT-bound nodes stay clients. On loopback, AutoNAT can't
	// confirm reachability, so force ModeServer there — otherwise no node serves the table and
	// resolve-by-key finds nothing.
	dhtMode := dht.ModeAuto
	if cfg.AllowPrivateAddrs {
		dhtMode = dht.ModeServer
	}
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

	// Every kernel offers the circuit-relay service. On a NAT-bound node it is unreachable and
	// idle (harmless); on a publicly-reachable node it automatically becomes the relay that lets
	// NAT-bound peers be reached — so a public `juice serve` is the network's meeting point, with
	// no separate seed process. Resource limits are libp2p defaults.
	if r, rerr := relayv2.New(h); rerr == nil {
		t.relay = r
	}

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
	if t.relay != nil {
		_ = t.relay.Close()
	}
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
