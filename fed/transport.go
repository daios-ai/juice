// SPDX-License-Identifier: AGPL-3.0-only

package fed

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/daios-ai/juice/kernel"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/discovery"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multistream"
)

// This file is the libp2p transport implementation behind the fed.go seam: identity derivation,
// the host + DHT + relay lifecycle, the versioned §13 protocol streams, and reachability probing.
// The kernel never imports it; only cmd/juice wires it in.

// ---- Identity ----
//
// A kernel is named by its address, the Ethereum address of its one secp256k1 key, and by nothing
// else (P1). libp2p authenticates every connection with that key; this file is the one place an
// address meets a connection, so nothing above it ever sees a libp2p peer id. The transport keeps
// one lookup from address to peer, filled alike by seeds, discovery, and inbound peers.

// deriveHostKey converts the kernel's key into a libp2p private key. The mapping is deterministic
// and 1:1, so the libp2p identity is a pure function of the signing key.
func deriveHostKey(priv *ecdsa.PrivateKey) (libp2pcrypto.PrivKey, error) {
	if priv == nil || priv.D == nil {
		return nil, fmt.Errorf("fed: the kernel's key is not configured")
	}
	return libp2pcrypto.UnmarshalSecp256k1PrivateKey(priv.D.FillBytes(make([]byte, 32)))
}

// addressOfPeer is the address of the kernel key a peer id carries, or "" for an id that carries
// none. A secp256k1 peer id embeds the key inline, so this never needs the network.
func addressOfPeer(id peer.ID) string {
	pub, err := id.ExtractPublicKey()
	if err != nil || pub == nil || pub.Type() != libp2pcrypto.Secp256k1 {
		return ""
	}
	raw, err := pub.Raw()
	if err != nil {
		return ""
	}
	key, err := ethcrypto.DecompressPubkey(raw)
	if err != nil {
		return ""
	}
	return kernel.KernelAddressOf(key)
}

// learn enters a peer in the lookup and returns its address, "" for a peer that is no kernel.
func (t *Transport) learn(id peer.ID) string {
	addr := addressOfPeer(id)
	if addr != "" {
		t.knownMu.Lock()
		t.known[addr] = id
		t.knownMu.Unlock()
	}
	return addr
}

// named is a library error with the peer id it names replaced by the kernel's address, keeping
// the reason and what it wraps: libp2p names a peer by its id, which is never a kernel's name (P1).
func named(err error, id peer.ID, address string) error {
	if err == nil || id == "" {
		return err
	}
	msg := err.Error()
	for _, s := range []string{id.String(), id.ShortString()} {
		msg = strings.ReplaceAll(msg, s, address)
	}
	return renamedErr{msg: msg, err: err}
}

type renamedErr struct {
	msg string
	err error
}

func (e renamedErr) Error() string { return e.msg }
func (e renamedErr) Unwrap() error { return e.err }

// lookup has its own lock: Close holds mu while handlers drain, and a handler entering a peer here
// must not wait on it.
func (t *Transport) lookup(addr string) (peer.ID, bool) {
	t.knownMu.Lock()
	defer t.knownMu.Unlock()
	id, ok := t.known[addr]
	return id, ok
}

// ---- Seeds ----
//
// A seed line is the seed's address and the TCP endpoint it is met at: 0x…@host:port, the form
// Cosmos and Ethereum node lists use. A kernel that has never met the seed learns the key behind
// the address from the seed itself: one plain TCP connection runs libp2p's own secure handshake
// without naming a peer, and the key the seed proves must hash to the address. The connection is
// then closed and the normal dialer connects to the peer that key names. A seed must therefore
// listen on TCP, as a seed always does.

type seed struct{ address, host, port string }

// parseSeeds reads seed lines. A malformed line is a configuration error, not a seed to skip.
func parseSeeds(lines []string) ([]seed, error) {
	var out []seed
	for _, line := range lines {
		if line == "" {
			continue
		}
		addr, endpoint, ok := strings.Cut(line, "@")
		host, port, err := net.SplitHostPort(endpoint)
		if !ok || err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("fed: seed %q is not a seed line, 0x…@host:port; a world file written for an earlier juice names its seeds otherwise and must be replaced", line)
		}
		a, err := kernel.ParseKernelAddress(addr)
		if err != nil {
			return nil, fmt.Errorf("fed: seed %q: %w", line, err)
		}
		out = append(out, seed{address: a, host: host, port: port})
	}
	return out, nil
}

// tcpMultiaddr is the multiaddr the normal dialer reaches host:port by.
func tcpMultiaddr(host, port string) (multiaddr.Multiaddr, error) {
	proto := "dns"
	if ip := net.ParseIP(host); ip != nil {
		proto = "ip6"
		if ip.To4() != nil {
			proto = "ip4"
		}
	}
	return multiaddr.NewMultiaddr("/" + proto + "/" + host + "/tcp/" + port)
}

// meetSeed learns which peer a seed is, refusing one whose key is not the seed's address. It sends
// no request and keeps no connection.
func meetSeed(ctx context.Context, self libp2pcrypto.PrivKey, s seed) (peer.AddrInfo, error) {
	endpoint := net.JoinHostPort(s.host, s.port)
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	defer c.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(d)
	}
	if err := multistream.SelectProtoOrFail(noise.ID, c); err != nil {
		return peer.AddrInfo{}, fmt.Errorf("fed: seed %s does not speak libp2p: %w", endpoint, err)
	}
	tpt, err := noise.New(noise.ID, self, nil)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	// The library's own check compares the peer with an id named in advance, which a seed line does
	// not carry; the check made instead is against the address, below.
	st, err := tpt.WithSessionOptions(noise.DisablePeerIDCheck())
	if err != nil {
		return peer.AddrInfo{}, err
	}
	sc, err := st.SecureOutbound(ctx, c, "")
	if err != nil {
		return peer.AddrInfo{}, fmt.Errorf("fed: seed %s: %w", endpoint, err)
	}
	id := sc.RemotePeer()
	_ = sc.Close()
	if got := addressOfPeer(id); got != s.address {
		return peer.AddrInfo{}, fmt.Errorf("fed: the kernel at %s is %s, not the seed %s", endpoint, got, s.address)
	}
	ma, err := tcpMultiaddr(s.host, s.port)
	if err != nil {
		return peer.AddrInfo{}, err
	}
	return peer.AddrInfo{ID: id, Addrs: []multiaddr.Multiaddr{ma}}, nil
}

// seedSet is the seeds met so far, read by the DHT and the relay finder whenever they look for
// bootstrap peers or relays.
type seedSet struct {
	mu  sync.Mutex
	all []seed
	met map[string]peer.AddrInfo
}

func (ss *seedSet) infos() []peer.AddrInfo {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]peer.AddrInfo, 0, len(ss.met))
	for _, sd := range ss.all {
		if ai, ok := ss.met[sd.address]; ok {
			out = append(out, ai)
		}
	}
	return out
}

// meet meets every seed not yet met, at once, and returns those it met now.
func (ss *seedSet) meet(ctx context.Context, self libp2pcrypto.PrivKey) []peer.AddrInfo {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var now []peer.AddrInfo
	for _, sd := range ss.all {
		ss.mu.Lock()
		_, done := ss.met[sd.address]
		ss.mu.Unlock()
		if done {
			continue
		}
		wg.Add(1)
		go func(sd seed) {
			defer wg.Done()
			mctx, cancel := context.WithTimeout(ctx, seedTimeout)
			defer cancel()
			ai, err := meetSeed(mctx, self, sd)
			if err != nil {
				return
			}
			ss.mu.Lock()
			ss.met[sd.address] = ai
			ss.mu.Unlock()
			mu.Lock()
			now = append(now, ai)
			mu.Unlock()
		}(sd)
	}
	wg.Wait()
	return now
}

func (ss *seedSet) pending() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return len(ss.met) < len(ss.all)
}

// seedTimeout bounds one attempt to meet a seed; seedRetry is how long a seed that could not be met
// waits before it is tried again. A seed down at boot is met when it comes back.
const (
	seedTimeout = 10 * time.Second
	seedRetry   = 30 * time.Second
)

// Endpoint renders this kernel at one TCP endpoint as a seed line, 0x…@host:port.
func Endpoint(address, host, port string) string { return address + "@" + net.JoinHostPort(host, port) }

// ---- Host lifecycle ----

// StdPort is the standard Juice federation port, bound by default the way 8333 is bitcoin's and
// 30303 Ethereum's: a kernel is dialable at a known number, so a firewall rule or a port forward
// can be written before the kernel exists. Below the Linux ephemeral range (32768+) and uncommon.
// A second kernel on one host sets fed_listen_addrs; it does not get the port silently (claimPort).
const StdPort = 31313

// stdListenAddrs is what a kernel binds when its configuration names no addresses, over both
// transports on the one port: TCP for ordinary dialing, QUIC for the hole punching a kernel behind
// a home router needs.
func stdListenAddrs(port int) []string {
	return []string{
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
		fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
	}
}

// claimPort fails when anything already holds the port a kernel is about to bind. libp2p opens its
// sockets with SO_REUSEPORT, so its own bind of a held port SUCCEEDS and two kernels share it, each
// taking a share of the other's connections; the collision has to be detected before libp2p binds.
// These two sockets are plain, without that option, so the operating system refuses them whenever
// the port is held at all, and they are closed again immediately. They are IPv4 sockets, the family
// libp2p binds (stdListenAddrs): Go opens "tcp" or "udp" on 0.0.0.0 as a dual-stack IPv6 socket,
// which macOS binds beside an IPv4 socket holding the same port, so that probe would find a held
// port free there.
func claimPort(port int) error {
	held := func(what string, err error) error {
		return fmt.Errorf("fed: %s port %d is already in use by another program; give this kernel "+
			"its own federation addresses (fed_listen_addrs in config.json, or --fed-listen-addrs): %w",
			what, port, err)
	}
	l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return held("tcp", err)
	}
	_ = l.Close()
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return held("udp", err)
	}
	_ = pc.Close()
	return nil
}

// Transport is the running federation carrier: a libp2p host plus a Kademlia DHT for
// resolve-by-key, with the §13 protocols registered. It implements the outbound client
// methods (Call/Manifests/Gossip/Inspect) and serves inbound streams via Config.Handlers.
type Transport struct {
	host      host.Host
	dht       *dht.IpfsDHT
	disc      *drouting.RoutingDiscovery
	relay     *relayv2.Relay
	cfg       Config
	namespace string
	seeds     *seedSet
	knownMu   sync.Mutex
	known     map[string]peer.ID // address → peer, for every kernel met (see learn)

	// ctx is cancelled by Close. Inbound handlers derive from it, so a read a peer abandoned — or
	// one running at shutdown — is cancelled rather than left to finish against nobody.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
}

// option is an unexported construction override. The public New applies none; the transport tests
// use them to force a DHT client/server topology on loopback — where AllowPrivateAddrs otherwise
// makes every node a ModeServer, hiding exactly the client-mode reachability the production bug
// lived in. The public fed.Config / fed.New API stays unchanged.
type option func(*buildOptions)

type buildOptions struct {
	dhtMode    dht.ModeOpt
	dhtModeSet bool
	stdPort    int // the standard port, overridden by tests that must not bind the real one
}

func withDHTMode(m dht.ModeOpt) option {
	return func(o *buildOptions) { o.dhtMode = m; o.dhtModeSet = true }
}

// withStdPort moves the standard port for one transport, so a test can exercise the real
// production path — claim the port, then bind it — without binding the port of a kernel this
// machine may be running.
func withStdPort(p int) option {
	return func(o *buildOptions) { o.stdPort = p }
}

// New builds and starts a transport host from cfg. It listens, connects to the bootstrap peers,
// starts the DHT and routing discovery, and registers the inbound protocol handlers. Call Close to stop.
func New(ctx context.Context, cfg Config) (*Transport, error) {
	return newTransport(ctx, cfg)
}

func newTransport(ctx context.Context, cfg Config, opts ...option) (*Transport, error) {
	bo := buildOptions{stdPort: StdPort}
	for _, opt := range opts {
		opt(&bo)
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("fed: discovery namespace is required")
	}
	hostKey, err := deriveHostKey(cfg.SigningKey)
	if err != nil {
		return nil, err
	}

	seeds, err := parseSeeds(cfg.BootstrapPeers)
	if err != nil {
		return nil, err
	}
	ss := &seedSet{all: seeds, met: map[string]peer.AddrInfo{}}
	ss.meet(ctx, hostKey)

	baseOpts := []libp2p.Option{
		libp2p.Identity(hostKey),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.NATPortMap(),
	}
	// What one peer may have in flight here at once. A token bucket bounds how fast requests
	// arrive; it does not bound how many are being served, and a handler may hold a frame, the
	// object it decoded and the reply it is building at the same time. The two controls are
	// orthogonal, so both are set: the bucket paces arrivals, these bound concurrent work. A
	// boundary that cannot be built is not a boundary: the transport refuses to start rather than
	// serve without it (D12, D20).
	mgr, err := boundedResources(orDefault(cfg.MaxInboundPeers, DefaultMaxInboundPeers))
	if err != nil {
		return nil, fmt.Errorf("fed: resource limits: %w", err)
	}
	baseOpts = append(baseOpts, libp2p.ResourceManager(mgr))
	if cfg.AgentVersion != "" {
		baseOpts = append(baseOpts, libp2p.UserAgent(cfg.AgentVersion))
	}
	if n := len(seeds); n > 0 {
		// The seeds are the relays, exactly as static relays are — the same candidate counts — read
		// from the seeds met so far, so a seed met after boot becomes a relay as well.
		met := func(_ context.Context, num int) <-chan peer.AddrInfo {
			infos := ss.infos()
			if len(infos) > num {
				infos = infos[:num]
			}
			c := make(chan peer.AddrInfo, len(infos))
			for _, ai := range infos {
				c <- ai
			}
			close(c)
			return c
		}
		baseOpts = append(baseOpts, libp2p.EnableAutoRelayWithPeerSource(met,
			autorelay.WithMaxCandidates(n), autorelay.WithMinCandidates(n), autorelay.WithNumRelays(n)))
	}

	build := func(listen []string) (host.Host, error) {
		return libp2p.New(append(baseOpts, libp2p.ListenAddrStrings(listen...))...)
	}

	// A caller-supplied ListenAddrs is bound verbatim and unprobed: the operator chose it, and a
	// failure to bind it is libp2p's to report. Loopback mode runs a whole network on one host, so
	// there the default is OS-assigned ports. Otherwise the standard port, claimed first so a
	// second kernel on this host is refused rather than silently sharing it (§13, D12).
	var h host.Host
	switch {
	case len(cfg.ListenAddrs) > 0:
		h, err = build(cfg.ListenAddrs)
	case cfg.AllowPrivateAddrs:
		h, err = build([]string{"/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"})
	default:
		if err = claimPort(bo.stdPort); err != nil {
			return nil, err
		}
		h, err = build(stdListenAddrs(bo.stdPort))
	}
	if err != nil {
		return nil, fmt.Errorf("fed: build host: %w", err)
	}

	// ModeAutoServer in production: serve the DHT (hold the routing table, answer FindPeer) while
	// reachability is still unknown, and downgrade to client only once AutoNAT confirms this node is
	// private. Plain ModeAuto starts as a client and promotes only after a ReachabilityPublic event,
	// which never arrives in a sparse network with no confirmer — leaving the seed a client that
	// serves no routing table, so resolve-by-key finds nothing (the confirmed federation bug, §13).
	// On loopback, AutoNAT can't confirm reachability at all, so force full ModeServer there.
	dhtMode := dht.ModeAutoServer
	if cfg.AllowPrivateAddrs {
		dhtMode = dht.ModeServer
	}
	if bo.dhtModeSet {
		dhtMode = bo.dhtMode // test-only override to build a real client/server topology (§15)
	}
	dhtOpts := []dht.Option{dht.Mode(dhtMode), dht.BootstrapPeersFunc(ss.infos)}
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

	tctx, tcancel := context.WithCancel(context.Background())
	t := &Transport{host: h, dht: kdht, disc: drouting.NewRoutingDiscovery(kdht), cfg: cfg,
		namespace: cfg.Namespace, seeds: ss, known: map[string]peer.ID{}, ctx: tctx, cancel: tcancel}

	// Every kernel offers the circuit-relay service. On a NAT-bound node it is unreachable and
	// idle (harmless); on a publicly-reachable node it automatically becomes the relay that lets
	// NAT-bound peers be reached — so a public `juice serve` is the network's meeting point, with
	// no separate seed process.
	if r, rerr := relayv2.New(h, relayv2.WithResources(relayResources(orDefault(cfg.RelaySlots, DefaultRelaySlots)))); rerr == nil {
		t.relay = r
	}

	// Connect to the seeds met so discovery and relay reservations can proceed; a seed not met yet
	// is tried again until it is.
	for _, ai := range ss.infos() {
		t.connectSeed(ctx, ai)
	}
	if ss.pending() {
		go t.meetLateSeeds(hostKey)
	}

	t.registerHandlers()
	return t, nil
}

// connectSeed connects to a seed met, entering it in the lookup.
func (t *Transport) connectSeed(ctx context.Context, ai peer.AddrInfo) {
	t.learn(ai.ID)
	cctx, cancel := context.WithTimeout(ctx, seedTimeout)
	_ = t.host.Connect(cctx, ai)
	cancel()
}

// meetLateSeeds keeps trying the seeds not met at boot until every one is, or the transport closes.
func (t *Transport) meetLateSeeds(self libp2pcrypto.PrivKey) {
	tick := time.NewTicker(seedRetry)
	defer tick.Stop()
	for t.seeds.pending() {
		select {
		case <-t.ctx.Done():
			return
		case <-tick.C:
		}
		for _, ai := range t.seeds.meet(t.ctx, self) {
			t.connectSeed(t.ctx, ai)
			t.dht.RefreshRoutingTable()
		}
	}
}

// Address is this kernel's address, its one identifier (P1).
func (t *Transport) Address() string { return addressOfPeer(t.host.ID()) }

// Endpoints are the TCP endpoints this host listens on, each written as a seed line. A line has the
// form of a seed, but whether another machine can reach it depends on where that machine is: a
// kernel behind a router is met through the network by its address instead.
func (t *Transport) Endpoints() []string {
	var out []string
	self := t.Address()
	for _, a := range t.host.Addrs() {
		// Only a plain TCP listener, host and port and nothing over them, is one a seed can be met
		// at: meeting a seed is a TCP handshake (meetSeed), which a WebSocket or relayed listener on
		// the same port does not answer.
		port, err := a.ValueForProtocol(multiaddr.P_TCP)
		if err != nil || len(a.Protocols()) != 2 {
			continue
		}
		for _, p := range []int{multiaddr.P_IP4, multiaddr.P_IP6, multiaddr.P_DNS, multiaddr.P_DNS4, multiaddr.P_DNS6} {
			if host, err := a.ValueForProtocol(p); err == nil {
				out = append(out, Endpoint(self, host, port))
				break
			}
		}
	}
	return out
}

// Close stops the DHT and host.
// ConnectedPeers is how many peers this host holds a connection to right now.
func (t *Transport) ConnectedPeers() int { return len(t.host.Network().Peers()) }

func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	t.cancel()
	if t.relay != nil {
		_ = t.relay.Close()
	}
	_ = t.dht.Close()
	return t.host.Close()
}

// resolve maps an address to a connected peer — the lookup first, the network otherwise — then
// ensures a connection (direct, hole-punched, or relayed) is open.
func (t *Transport) resolve(ctx context.Context, address string) (peer.ID, error) {
	address, err := kernel.ParseKernelAddress(address)
	if err != nil {
		return "", err
	}
	pid, known := t.lookup(address)
	if known {
		if t.host.Network().Connectedness(pid) == network.Connected {
			return pid, nil
		}
		if addrs := t.host.Peerstore().Addrs(pid); len(addrs) > 0 {
			if err := t.host.Connect(ctx, peer.AddrInfo{ID: pid, Addrs: addrs}); err == nil {
				return pid, nil
			}
		}
	}
	// Resolve via the DHT: a kernel never met by the address it announces itself under, one met
	// before by its peer. On a freshly-formed network the routing table may not yet carry the
	// target, so retry a few times while the DHT settles (bounded by the caller's context).
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		var ai peer.AddrInfo
		if known {
			ai, err = t.dht.FindPeer(ctx, pid)
		} else {
			ai, err = t.findByAddress(ctx, address)
		}
		if err == nil && len(ai.Addrs) == 0 && ai.ID != "" {
			ai, err = t.dht.FindPeer(ctx, ai.ID)
		}
		if err == nil && len(ai.Addrs) > 0 {
			pid, known = ai.ID, true
			t.host.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.TempAddrTTL)
			if cerr := t.host.Connect(ctx, ai); cerr == nil {
				return pid, nil
			} else {
				lastErr = named(cerr, pid, address)
			}
		} else if err != nil {
			lastErr = named(err, pid, address)
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("fed: cannot resolve kernel %s: %w", address, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("not found")
	}
	return "", fmt.Errorf("fed: cannot resolve kernel %s: %w", address, lastErr)
}

// addressNamespace is what a kernel announces itself under so it is found by its address alone.
func (t *Transport) addressNamespace(address string) string {
	return t.namespace + "/kernel/" + address
}

// findByAddress asks the network which peers announce themselves under address and keeps the one
// whose key is that address. Anyone may announce under any address; no one else's key hashes to it,
// so a false announcer can delay a lookup but never redirect it.
func (t *Transport) findByAddress(ctx context.Context, address string) (peer.AddrInfo, error) {
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := t.disc.FindPeers(fctx, t.addressNamespace(address), discovery.Limit(0))
	if err != nil {
		return peer.AddrInfo{}, err
	}
	for ai := range ch {
		if addressOfPeer(ai.ID) == address {
			t.learn(ai.ID)
			return ai, nil
		}
	}
	return peer.AddrInfo{}, fmt.Errorf("no kernel announces itself as %s", address)
}

// ---- Discovery (§13) ----
//
// Discovery is libp2p routing discovery over a fixed namespace: every kernel advertises the namespace
// to the DHT (its provider record carries its transport addresses) and enumerates the namespace's
// providers to learn other kernels — addresses included, refreshed into the peerstore. A DHT client
// may both advertise and enumerate, so a NAT-bound kernel is findable through the public servers with
// no home-grown membership protocol. Provider records are ephemeral transport data and grant no Juice
// identity, credit, callability, or alias; a verified first-party gossip pull is what a kernel
// actually believes (§13).

// discoveryLimit bounds the providers enumerated per pass, so one enumeration cannot be made
// unboundedly expensive by a large (or flooded) namespace.
const discoveryLimit = 100

// Advertise announces this kernel as a provider of the discovery namespace and of its own address,
// publishing its current transport addresses to the DHT. Called once per discovery pass (§13).
// Provide needs only query capability, so a DHT-client kernel behind NAT advertises successfully
// and becomes findable through the public servers, by the network and by its address.
func (t *Transport) Advertise(ctx context.Context) error {
	if _, err := t.disc.Advertise(ctx, t.namespace); err != nil {
		return err
	}
	_, err := t.disc.Advertise(ctx, t.addressNamespace(t.Address()))
	return err
}

// DiscoverProviders enumerates the discovery namespace's providers, refreshes each provider's
// addresses into the peerstore as ephemeral reachability data (never Juice identity), and returns
// their addresses — this kernel's own excluded. It is the routing-discovery
// membership feed the discovery loop pulls gossip from; a returned key grants nothing until a
// verified first-party gossip pull (§13).
func (t *Transport) DiscoverProviders(ctx context.Context) ([]string, error) {
	infos, err := dutil.FindPeers(ctx, t.disc, t.namespace, discovery.Limit(discoveryLimit))
	if err != nil {
		return nil, err
	}
	self := t.host.ID()
	var out []string
	for _, ai := range infos {
		if ai.ID == self {
			continue
		}
		if len(ai.Addrs) > 0 {
			t.host.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.TempAddrTTL)
		}
		if a := t.learn(ai.ID); a != "" {
			out = append(out, a)
		}
	}
	return out, nil
}

// SeedAddresses are the addresses of the configured seeds, which the discovery loop seeds from and
// an operator can inspect. A seed line names its address, so this needs no network.
func (t *Transport) SeedAddresses() []string {
	var out []string
	for _, sd := range t.seeds.all {
		out = append(out, sd.address)
	}
	return out
}

// ---- Protocol streams ----
//
// Wire framing: each message is a 4-byte big-endian length prefix followed by that many JSON
// bytes. Every protocol is a single request frame and response frame.

const (
	maxFrameBytes  = 8 << 20 // 8 MiB per frame — a call+receipt or one action manifest fits comfortably
	streamDeadline = 60 * time.Second
)

func writeFrame(s io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrameBytes {
		return fmt.Errorf("fed: frame too large (%d bytes)", len(b))
	}
	var hdr [4]byte
	hdr[0] = byte(len(b) >> 24)
	hdr[1] = byte(len(b) >> 16)
	hdr[2] = byte(len(b) >> 8)
	hdr[3] = byte(len(b))
	if _, err := s.Write(hdr[:]); err != nil {
		return err
	}
	_, err = s.Write(b)
	return err
}

func readFrame(s io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(s, hdr[:]); err != nil {
		return err
	}
	n := int(hdr[0])<<24 | int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n < 0 || n > maxFrameBytes {
		return fmt.Errorf("fed: frame length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// peerOf returns the address of the stream's remote peer and enters it in the lookup. The
// connection is Noise-authenticated by libp2p, so the address is proven, not claimed.
func (t *Transport) peerOf(s network.Stream) string { return t.learn(s.Conn().RemotePeer()) }

func (t *Transport) registerHandlers() {
	t.host.SetStreamHandler(protocol.ID(ProtocolCall), t.handleCall)
	t.host.SetStreamHandler(protocol.ID(ProtocolResolve), t.handleResolve)
	t.host.SetStreamHandler(protocol.ID(ProtocolGossip), t.handleGossip)
	t.host.SetStreamHandler(protocol.ID(ProtocolTask), t.handleTask)
	t.host.SetStreamHandler(protocol.ID(ProtocolReveal), t.handleReveal)
	t.host.SetStreamHandler(protocol.ID(ProtocolTransfer), t.handleTransfer)
	t.host.SetStreamHandler(protocol.ID(ProtocolGrant), t.handleGrant)
	t.host.SetStreamHandler(protocol.ID(ProtocolProcess), t.handleProcess)
}

// streamsPerPeer and streamsSystemWide bound inbound concurrency [policy]; the connections behind
// them are the operator's number (Config.MaxInboundPeers), plus connsOutboundHeadroom for this
// kernel's own dialling. They are chosen, not derived: a bound follows from what this kernel is
// willing to hold at once, and the frame size only says what one stream may cost. The resource
// manager is libp2p's own — a standard bulkhead, not a semaphore of ours — and its per-address
// limits are left as they come. The two defaults are what an ordinary kernel runs under.
const (
	streamsPerPeer         = 8
	streamsSystemWide      = 64
	connsOutboundHeadroom  = 192
	DefaultMaxInboundPeers = 64
	DefaultRelaySlots      = 128
)

// orDefault is what "unset takes the default" means for every configured limit.
func orDefault(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}

// boundedResources is the default limit set with those numbers written over it, so everything
// else libp2p bounds stays bounded the way libp2p bounds it.
var boundedResources = func(inbound int) (network.ResourceManager, error) {
	scaling := rcmgr.DefaultLimits
	libp2p.SetDefaultServiceLimits(&scaling)
	limits := rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			StreamsInbound: streamsSystemWide,
			ConnsInbound:   rcmgr.LimitVal(inbound),
			Conns:          rcmgr.LimitVal(inbound + connsOutboundHeadroom),
		},
		PeerDefault: rcmgr.ResourceLimits{StreamsInbound: streamsPerPeer},
	}.Build(scaling.AutoScale())
	return rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(limits))
}

// relayResources is libp2p's relay default with the slot count written over it and the data
// allowance raised to two frames, since a relayed circuit must carry what a direct one carries
// (U34) and the default 128 KiB per direction is a fraction of one frame (D12).
func relayResources(slots int) relayv2.Resources {
	r := relayv2.DefaultResources()
	r.MaxReservations = slots
	limit := *r.Limit
	limit.Data = 2 * maxFrameBytes
	r.Limit = &limit
	return r
}

// serveReq reads one request, answers it, and closes. The handler's context carries the stream's
// own deadline and dies with the transport, so an answer nobody is waiting for — an abandoned
// read, a shutdown — stops instead of running on against a closed stream. A request counts as
// served only once its answer is written.
func serveReq[Req any](t *Transport, s network.Stream, handle func(context.Context, string, Req) any) {
	defer s.Close()
	ok := false
	defer func() { t.observe(string(s.Protocol()), "in", ok) }()
	deadline := time.Now().Add(streamDeadline)
	_ = s.SetDeadline(deadline)
	hctx, cancel := context.WithDeadline(t.ctx, deadline)
	defer cancel()
	var req Req
	if err := readFrame(s, &req); err != nil {
		return
	}
	// A handler with nothing to say says nothing: the stream closes with no frame, which the
	// caller reads as a failed read rather than as an answer.
	if reply := handle(hctx, t.peerOf(s), req); reply != nil {
		ok = writeFrame(s, reply) == nil
	}
}

// observe reports one request's end to the configured observer, if any.
func (t *Transport) observe(proto, direction string, ok bool) {
	if t.cfg.Observe != nil {
		t.cfg.Observe(proto, direction, ok)
	}
}

func (t *Transport) handleCall(s network.Stream) {
	serveReq(t, s, func(ctx context.Context, key string, req CallRequest) any {
		return t.cfg.Handlers.OnCall(ctx, key, req)
	})
}

func (t *Transport) handleTask(s network.Stream) {
	serveReq(t, s, func(ctx context.Context, key string, req TaskRequest) any {
		return t.cfg.Handlers.OnTask(ctx, key, req)
	})
}

func (t *Transport) handleResolve(s network.Stream) {
	serveReq(t, s, func(ctx context.Context, key string, req ResolveRequest) any {
		return t.cfg.Handlers.OnResolve(ctx, key, req)
	})
}

func (t *Transport) handleReveal(s network.Stream) {
	serveReq(t, s, func(ctx context.Context, key string, req RevealRequest) any {
		return t.cfg.Handlers.OnReveal(ctx, key, req)
	})
}

func (t *Transport) handleTransfer(s network.Stream) {
	serveReq(t, s, func(ctx context.Context, key string, req TransferRequest) any {
		return t.cfg.Handlers.OnTransfer(ctx, key, req)
	})
}

func (t *Transport) handleGrant(s network.Stream) {
	serveReq(t, s, func(ctx context.Context, key string, req GrantRequest) any {
		return t.cfg.Handlers.OnGrant(ctx, key, req)
	})
}

func (t *Transport) handleProcess(s network.Stream) {
	serveReq(t, s, func(ctx context.Context, key string, req ProcessRequest) any {
		return t.cfg.Handlers.OnProcess(ctx, key, req)
	})
}

func (t *Transport) handleGossip(s network.Stream) {
	// Gossip carries the evidence cursor and the catalogue scan (§13). The reply frame is the JSON
	// document; on handler error we close without a frame, which the client reads as empty.
	serveReq(t, s, func(ctx context.Context, key string, req GossipRequest) any {
		body, err := t.cfg.Handlers.OnGossip(ctx, key, req)
		if err != nil {
			return nil
		}
		return body
	})
}

// ---- Outbound client ----

func (t *Transport) openStream(ctx context.Context, peerKey, proto string) (network.Stream, error) {
	pid, err := t.resolve(ctx, peerKey)
	if err != nil {
		// Resolve/connect failed before any request byte was written: provably never sent (§13).
		return nil, fmt.Errorf("%w: %v", ErrNotDispatched, err)
	}
	// Allow dialing a relayed connection when no direct path exists.
	sctx := network.WithAllowLimitedConn(ctx, "juice-fed")
	s, err := t.host.NewStream(sctx, pid, protocol.ID(proto))
	if err != nil {
		return nil, fmt.Errorf("fed: open %s to %s: %w", proto, peerKey, named(err, pid, peerKey))
	}
	// The caller's own deadline wins when it is the shorter one: a pass that budgets ten seconds
	// per peer must not wait a minute because the stream's default says it may.
	deadline := time.Now().Add(streamDeadline)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = s.SetDeadline(deadline)
	return s, nil
}

// roundTrip opens a stream for one request/response protocol, writes req, and reads the reply. On
// any error it returns the zero Resp (e.g. an empty CallResponse the caller treats as pending).
// Every request is reported to the observer once, here, whichever protocol carried it.
func roundTrip[Req, Resp any](ctx context.Context, t *Transport, peerKey, proto string, req Req) (Resp, error) {
	resp, err := exchange[Req, Resp](ctx, t, peerKey, proto, req)
	t.observe(proto, "out", err == nil)
	return resp, err
}

func exchange[Req, Resp any](ctx context.Context, t *Transport, peerKey, proto string, req Req) (Resp, error) {
	var resp Resp
	s, err := t.openStream(ctx, peerKey, proto)
	if err != nil {
		return resp, err
	}
	defer s.Close()
	// A cancelled caller stops the read now rather than at the deadline: resetting the stream is
	// what makes the blocked Read return.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Reset()
		case <-done:
		}
	}()
	if err := writeFrame(s, req); err != nil {
		return resp, ourOwnCancel(ctx, fmt.Errorf("fed: write %s: %w", proto, err))
	}
	if err := readFrame(s, &resp); err != nil {
		return resp, ourOwnCancel(ctx, fmt.Errorf("fed: read %s: %w", proto, err))
	}
	return resp, nil
}

// ourOwnCancel names the caller's cancellation as the cause when there is one: the stream we reset
// ourselves must not read to a caller as the peer breaking the connection.
func ourOwnCancel(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %w", cerr, err)
	}
	return err
}

// Call sends a federation call to the peer and returns its settlement envelope.
func (t *Transport) Call(ctx context.Context, peerKey string, req CallRequest) (CallResponse, error) {
	return roundTrip[CallRequest, CallResponse](ctx, t, peerKey, ProtocolCall, req)
}

// Task sends a task list/complete request to the peer (§13).
func (t *Transport) Task(ctx context.Context, peerKey string, req TaskRequest) (TaskResponse, error) {
	return roundTrip[TaskRequest, TaskResponse](ctx, t, peerKey, ProtocolTask, req)
}

// Resolve fetches one action's signed manifest or one user's stable id+handle from the peer (§13).
func (t *Transport) Resolve(ctx context.Context, peerKey string, req ResolveRequest) (ResolveResponse, error) {
	return roundTrip[ResolveRequest, ResolveResponse](ctx, t, peerKey, ProtocolResolve, req)
}

// Reveal tells the peer how one obligation's draw came out (P10).
func (t *Transport) Reveal(ctx context.Context, peerKey string, req RevealRequest) (RevealResponse, error) {
	return roundTrip[RevealRequest, RevealResponse](ctx, t, peerKey, ProtocolReveal, req)
}

// Transfer tells the peer whom a payment to it is for (P11).
func (t *Transport) Transfer(ctx context.Context, peerKey string, req TransferRequest) (TransferResponse, error) {
	return roundTrip[TransferRequest, TransferResponse](ctx, t, peerKey, ProtocolTransfer, req)
}

// Grant forwards one signed grant operation to the peer that serves the actions it names (D10).
func (t *Transport) Grant(ctx context.Context, peerKey string, req GrantRequest) (Response, error) {
	return roundTrip[GrantRequest, Response](ctx, t, peerKey, ProtocolGrant, req)
}

// Process asks the peer a run began on about that run's process, or tells it of work held (D2).
func (t *Transport) Process(ctx context.Context, peerKey string, req ProcessRequest) (Response, error) {
	return roundTrip[ProcessRequest, Response](ctx, t, peerKey, ProtocolProcess, req)
}

// Gossip fetches one page of the peer's gossip document, resuming from cursor (§13). An empty
// cursor starts at the oldest retained evidence; the catalog snapshot rides every reply.
func (t *Transport) Gossip(ctx context.Context, peerKey string, req GossipRequest) (json.RawMessage, error) {
	return roundTrip[GossipRequest, json.RawMessage](ctx, t, peerKey, ProtocolGossip, req)
}

// ---- Reachability ----

// Reachability describes how this transport can reach a peer right now — the diagnostic the
// operator sees via `admin inspect <key>` now that there is no browser-reachable endpoint.
type Reachability struct {
	Path      string   `json:"path"`              // "direct", "relayed", or "unreachable"
	RTTmillis int64    `json:"rtt_millis"`        // one libp2p ping round trip, milliseconds; a peer that does not answer is unreachable
	Protocols []string `json:"protocols"`         // libp2p protocols the peer advertises
	Version   string   `json:"version,omitempty"` // the Juice version the peer says it runs; empty when it says none
	Error     string   `json:"error,omitempty"`
}

// Probe resolves and connects to a peer by address, classifies the connection, and measures one round
// trip with libp2p's ping. It does not send a federation call. Connecting is not the round trip: on
// a connection already held it costs nothing, so the time reported is the ping's alone.
func (t *Transport) Probe(ctx context.Context, peerKey string) Reachability {
	pid, err := t.resolve(ctx, peerKey)
	if err != nil {
		return Reachability{Path: "unreachable", Error: err.Error()}
	}
	// Ping repeats until its context ends; one answer is all a probe asks for. A peer that does not
	// answer is unreachable, whatever connection is held to it.
	pctx, stop := context.WithCancel(ctx)
	rtt, err := firstPing(ping.Ping(pctx, t.host, pid))
	stop()
	if err != nil {
		return Reachability{Path: "unreachable", Error: "ping: " + named(err, pid, peerKey).Error()}
	}
	r := Reachability{RTTmillis: rtt.Milliseconds()}
	// Classify the live connection: a limited (relayed) connection routes through a relay.
	r.Path = "direct"
	for _, c := range t.host.Network().ConnsToPeer(pid) {
		if c.Stat().Limited {
			r.Path = "relayed"
			break
		}
	}
	if protos, err := t.host.Peerstore().GetProtocols(pid); err == nil {
		for _, p := range protos {
			r.Protocols = append(r.Protocols, string(p))
		}
	}
	// Identify stored the peer's agent string under this key when the connection was made.
	if av, err := t.host.Peerstore().Get(pid, "AgentVersion"); err == nil {
		if s, ok := av.(string); ok && strings.HasPrefix(s, AgentPrefix) {
			r.Version = s
		}
	}
	return r
}

// firstPing reads one ping result. A channel closed without one — the probe's time ran out mid-ping
// — is no answer, not a round trip of zero.
func firstPing(ch <-chan ping.Result) (time.Duration, error) {
	res, ok := <-ch
	if !ok {
		return 0, errors.New("no answer")
	}
	return res.RTT, res.Error
}
