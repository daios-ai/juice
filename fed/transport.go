// SPDX-License-Identifier: AGPL-3.0-only

package fed

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

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
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/multiformats/go-multiaddr"
)

// This file is the libp2p transport implementation behind the fed.go seam: identity derivation,
// the host + DHT + relay lifecycle, the versioned §13 protocol streams, and reachability probing.
// The kernel never imports it; only cmd/juice wires it in.

// ---- Identity ----
//
// The federation network identity is derived deterministically from the platform Ed25519 signing
// key — there is no second key (§12). A peer's base64url Ed25519 public key is simultaneously its
// Juice identity (§13) and its address on this transport.

// deriveHostKey converts a standard library ed25519 private key into a libp2p private key. The
// mapping is deterministic and 1:1, so the libp2p peer ID is a pure function of the signing key.
func deriveHostKey(priv ed25519.PrivateKey) (libp2pcrypto.PrivKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("fed: signing key must be a %d-byte ed25519 private key, got %d", ed25519.PrivateKeySize, len(priv))
	}
	return libp2pcrypto.UnmarshalEd25519PrivateKey(priv)
}

// PeerIDFromKey maps a base64url Ed25519 public key (the identity used everywhere in §13) to the
// libp2p peer ID the transport dials. Deterministic and 1:1.
func PeerIDFromKey(publicKeyB64 string) (peer.ID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return "", fmt.Errorf("fed: public key must be base64url: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return "", fmt.Errorf("fed: public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	pub, err := libp2pcrypto.UnmarshalEd25519PublicKey(raw)
	if err != nil {
		return "", fmt.Errorf("fed: invalid ed25519 public key: %w", err)
	}
	return peer.IDFromPublicKey(pub)
}

// KeyFromPeerID recovers the base64url Ed25519 public key from a libp2p peer ID. Ed25519 peer IDs
// embed the public key inline, so this never needs the network.
func KeyFromPeerID(id peer.ID) (string, error) {
	pub, err := id.ExtractPublicKey()
	if err != nil || pub == nil {
		return "", fmt.Errorf("fed: peer ID does not embed a public key: %w", err)
	}
	raw, err := pub.Raw()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

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
// the port is held at all, and they are closed again immediately.
func claimPort(port int) error {
	held := func(what string, err error) error {
		return fmt.Errorf("fed: %s port %d is already in use by another program; give this kernel "+
			"its own federation addresses (fed_listen_addrs in config.json, or --fed-listen-addrs): %w",
			what, port, err)
	}
	l, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return held("tcp", err)
	}
	_ = l.Close()
	pc, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", port))
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
	bootstrap []peer.AddrInfo

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

	t := &Transport{host: h, dht: kdht, disc: drouting.NewRoutingDiscovery(kdht), cfg: cfg, namespace: cfg.Namespace, bootstrap: bootstrap}

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

// ListenAddrs returns the multiaddrs (including /p2p/<id>) the host is reachable on. Used for
// logging at server.ready and for the flow harness to scrape a seed's address.
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

// resolve maps a base64url public key to a connected peer, using the peerstore first and the DHT
// otherwise, then ensures a connection (direct, hole-punched, or relayed) is open.
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

// Advertise announces this kernel as a provider of the discovery namespace, publishing its current
// transport addresses to the DHT, and returns the TTL after which the record should be refreshed.
// Called once per discovery pass (§13). Provide needs only query capability, so a DHT-client kernel
// behind NAT advertises successfully and becomes findable through the public servers.
func (t *Transport) Advertise(ctx context.Context) (time.Duration, error) {
	return t.disc.Advertise(ctx, t.namespace)
}

// DiscoverProviders enumerates the discovery namespace's providers, refreshes each provider's
// addresses into the peerstore as ephemeral reachability data (never Juice identity), and returns
// their base64url public keys — this kernel's own key excluded. It is the routing-discovery
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
		if k, err := KeyFromPeerID(ai.ID); err == nil {
			out = append(out, k)
		}
	}
	return out, nil
}

// BootstrapKeys returns the base64url Ed25519 keys of the configured bootstrap peers — the same
// key format inspect/gossip take. The libp2p peer ID inlines the Ed25519 key (KeyFromPeerID), so
// this is the whole peer-ID→key bridge: the discovery loop seeds from, and an operator can inspect,
// a bootstrap node addressed only by its multiaddr.
func (t *Transport) BootstrapKeys() []string {
	var out []string
	for _, ai := range t.bootstrap {
		if k, err := KeyFromPeerID(ai.ID); err == nil {
			out = append(out, k)
		}
	}
	return out
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

// peerKeyOf returns the base64url public key of the stream's remote peer. The connection is
// Noise-authenticated by libp2p, so this key is proven, not claimed.
func peerKeyOf(s network.Stream) string {
	k, _ := KeyFromPeerID(s.Conn().RemotePeer())
	return k
}

func (t *Transport) registerHandlers() {
	t.host.SetStreamHandler(protocol.ID(ProtocolCall), t.handleCall)
	t.host.SetStreamHandler(protocol.ID(ProtocolResolve), t.handleResolve)
	t.host.SetStreamHandler(protocol.ID(ProtocolGossip), t.handleGossip)
	t.host.SetStreamHandler(protocol.ID(ProtocolStep), t.handleStep)
	t.host.SetStreamHandler(protocol.ID(ProtocolReveal), t.handleReveal)
}

// serveReq reads one typed request frame, runs handle, and writes its response frame. Used by the
// request/response protocols (call, step).
func serveReq[Req any](s network.Stream, handle func(string, Req) any) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	var req Req
	if err := readFrame(s, &req); err != nil {
		return
	}
	_ = writeFrame(s, handle(peerKeyOf(s), req))
}

func (t *Transport) handleCall(s network.Stream) {
	serveReq(s, func(key string, req CallRequest) any { return t.cfg.Handlers.OnCall(context.Background(), key, req) })
}

func (t *Transport) handleStep(s network.Stream) {
	serveReq(s, func(key string, req StepRequest) any { return t.cfg.Handlers.OnStep(context.Background(), key, req) })
}

func (t *Transport) handleResolve(s network.Stream) {
	serveReq(s, func(key string, req ResolveRequest) any {
		return t.cfg.Handlers.OnResolve(context.Background(), key, req)
	})
}

func (t *Transport) handleReveal(s network.Stream) {
	serveReq(s, func(key string, req RevealRequest) any {
		return t.cfg.Handlers.OnReveal(context.Background(), key, req)
	})
}

func (t *Transport) handleGossip(s network.Stream) {
	// Gossip is now a request/response protocol carrying the evidence cursor (§13). The reply frame is
	// the JSON document; on handler error we close without a frame, which the client reads as empty.
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	var req GossipRequest
	if err := readFrame(s, &req); err != nil {
		return
	}
	body, err := t.cfg.Handlers.OnGossip(context.Background(), peerKeyOf(s), req)
	if err != nil {
		return
	}
	_ = writeFrame(s, body)
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
		return nil, fmt.Errorf("fed: open %s to %s: %w", proto, pid, err)
	}
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	return s, nil
}

// roundTrip opens a stream for one request/response protocol, writes req, and reads the reply. On
// any error it returns the zero Resp (e.g. an empty CallResponse the caller treats as pending).
func roundTrip[Req, Resp any](ctx context.Context, t *Transport, peerKey, proto string, req Req) (Resp, error) {
	var resp Resp
	s, err := t.openStream(ctx, peerKey, proto)
	if err != nil {
		return resp, err
	}
	defer s.Close()
	if err := writeFrame(s, req); err != nil {
		return resp, fmt.Errorf("fed: write %s: %w", proto, err)
	}
	if err := readFrame(s, &resp); err != nil {
		return resp, fmt.Errorf("fed: read %s: %w", proto, err)
	}
	return resp, nil
}

// Call sends a federation call to the peer and returns its settlement envelope.
func (t *Transport) Call(ctx context.Context, peerKey string, req CallRequest) (CallResponse, error) {
	return roundTrip[CallRequest, CallResponse](ctx, t, peerKey, ProtocolCall, req)
}

// Step sends a step list/complete request to the peer (§13).
func (t *Transport) Step(ctx context.Context, peerKey string, req StepRequest) (StepResponse, error) {
	return roundTrip[StepRequest, StepResponse](ctx, t, peerKey, ProtocolStep, req)
}

// Resolve fetches one action's signed manifest or one user's stable id+handle from the peer (§13).
func (t *Transport) Resolve(ctx context.Context, peerKey string, req ResolveRequest) (ResolveResponse, error) {
	return roundTrip[ResolveRequest, ResolveResponse](ctx, t, peerKey, ProtocolResolve, req)
}

// Reveal tells the peer how one obligation's draw came out (P10).
func (t *Transport) Reveal(ctx context.Context, peerKey string, req RevealRequest) (RevealResponse, error) {
	return roundTrip[RevealRequest, RevealResponse](ctx, t, peerKey, ProtocolReveal, req)
}

// Gossip fetches one page of the peer's gossip document, resuming from cursor (§13). An empty
// cursor starts at the oldest retained evidence; the catalog snapshot rides every reply.
func (t *Transport) Gossip(ctx context.Context, peerKey, cursor string) (json.RawMessage, error) {
	return roundTrip[GossipRequest, json.RawMessage](ctx, t, peerKey, ProtocolGossip, GossipRequest{Cursor: cursor})
}

// ---- Reachability ----

// Reachability describes how this transport can reach a peer right now — the diagnostic the
// operator sees via `admin inspect <key>` now that there is no browser-reachable endpoint.
type Reachability struct {
	Path      string   `json:"path"`       // "direct", "relayed", or "unreachable"
	RTTmillis int64    `json:"rtt_millis"` // round-trip time of the resolve+connect, milliseconds
	Protocols []string `json:"protocols"`  // libp2p protocols the peer advertises
	Error     string   `json:"error,omitempty"`
}

// Probe resolves and connects to a peer by key and reports the reachability path. It does not send
// a federation call — it only establishes (or reuses) a connection and classifies it.
func (t *Transport) Probe(ctx context.Context, peerKey string) Reachability {
	start := time.Now()
	pid, err := t.resolve(ctx, peerKey)
	if err != nil {
		return Reachability{Path: "unreachable", RTTmillis: time.Since(start).Milliseconds(), Error: err.Error()}
	}
	r := Reachability{RTTmillis: time.Since(start).Milliseconds()}
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
	return r
}

// isRelayAddr reports whether a multiaddr string denotes a circuit-relay path.
func isRelayAddr(s string) bool { return strings.Contains(s, "/p2p-circuit") }
