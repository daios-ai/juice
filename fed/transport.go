package fed

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
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

// StdPort is the standard Juice federation port. A publicly-reachable kernel that binds it has a
// stable, well-known address others can bootstrap to. Below the Linux ephemeral range (32768+),
// uncommon, and echoes Ethereum's 30303. If it is already taken, the transport falls back to an
// OS-assigned port (kernels are found by key via the DHT, so only a public bootstrap node needs
// the fixed one) and warns.
const StdPort = 31313

// Transport is the running federation carrier: a libp2p host plus a Kademlia DHT for
// resolve-by-key, with the §13 protocols registered. It implements the outbound client
// methods (Call/Manifests/Gossip/Inspect) and serves inbound streams via Config.Handlers.
type Transport struct {
	host      host.Host
	dht       *dht.IpfsDHT
	relay     *relayv2.Relay
	cfg       Config
	bootstrap []peer.AddrInfo

	mu     sync.Mutex
	closed bool
}

// New builds and starts a transport host from cfg. It listens, connects to the bootstrap peers,
// starts the DHT, and registers the inbound protocol handlers. Call Close to stop.
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

// ---- Discovery (the known-network directory engine, §13) ----
//
// Discovery is separate from gossip: gossip carries trade-backed reputation, while the DHT
// provider-record rendezvous below is the fast, broad directory. Every kernel advertises itself
// under one fixed content key and enumerates the same key to learn who else is online. Learning a
// kernel this way grants nothing (§13) — it only fills the address book; calling still needs a
// friendship and a deposit.

const discoveryRendezvous = "juice/kernel/discovery/1"

// juiceDiscoveryCID is the fixed content key every kernel provides and looks up to find peers. It
// is a pure function of discoveryRendezvous, so every kernel computes the same value with no
// coordination.
var juiceDiscoveryCID = mustDiscoveryCID()

func mustDiscoveryCID() cid.Cid {
	mh, err := multihash.Sum([]byte(discoveryRendezvous), multihash.SHA2_256, -1)
	if err != nil {
		panic(fmt.Sprintf("fed: discovery cid: %v", err))
	}
	return cid.NewCidV1(cid.Raw, mh)
}

// BootstrapKeys returns the base64url Ed25519 keys of the configured bootstrap peers — the same
// key format friend/inspect/gossip take. The libp2p peer ID inlines the Ed25519 key
// (KeyFromPeerID), so this is the whole peer-ID→key bridge: the discovery loop can seed from, and
// an operator can inspect, a bootstrap node addressed only by its multiaddr.
func (t *Transport) BootstrapKeys() []string {
	var out []string
	for _, ai := range t.bootstrap {
		if k, err := KeyFromPeerID(ai.ID); err == nil {
			out = append(out, k)
		}
	}
	return out
}

// Advertise announces this kernel under the fixed juice discovery key so peers enumerating it find
// us. Provider records expire, so the discovery loop re-advertises each pass. Best effort: an
// unreachable DHT returns an error the caller logs and ignores.
func (t *Transport) Advertise(ctx context.Context) error {
	return t.dht.Provide(ctx, juiceDiscoveryCID, true)
}

// DiscoverProviders enumerates kernels advertising the juice discovery key and returns their
// base64url keys (up to limit), excluding ourselves. This is the directory pull: it grows the
// known network at the rate kernels come online, independent of who we have friended or traded
// with.
func (t *Transport) DiscoverProviders(ctx context.Context, limit int) []string {
	self := t.host.ID()
	seen := map[string]bool{}
	var out []string
	for ai := range t.dht.FindProvidersAsync(ctx, juiceDiscoveryCID, limit) {
		if ai.ID == self {
			continue
		}
		k, err := KeyFromPeerID(ai.ID)
		if err != nil || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
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
// bytes. Manifest sync sends one frame per action so a large catalog survives a bandwidth-capped
// relayed connection; every other protocol is a single request frame and response frame.

const (
	maxFrameBytes  = 8 << 20 // 8 MiB per frame — a call+receipt or one action manifest fits comfortably
	streamDeadline = 60 * time.Second
	maxManifests   = 100000 // upper bound on a peer-declared manifest chunk count (anti-OOM)
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
	t.host.SetStreamHandler(protocol.ID(ProtocolManifest), t.handleManifest)
	t.host.SetStreamHandler(protocol.ID(ProtocolGossip), t.handleGossip)
	t.host.SetStreamHandler(protocol.ID(ProtocolInspect), t.handleInspect)
}

// serveReq reads one typed request frame, runs handle, and writes its response frame. Used by the
// request/response protocols (call, friend).
func serveReq[Req any](s network.Stream, handle func(string, Req) any) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	var req Req
	if err := readFrame(s, &req); err != nil {
		return
	}
	_ = writeFrame(s, handle(peerKeyOf(s), req))
}

// serveDoc writes the document produced by fetch. The document protocols (gossip, inspect) take no
// request payload, so the client's empty request frame is left unread and discarded on close.
func serveDoc(s network.Stream, fetch func(string) (json.RawMessage, error)) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	body, err := fetch(peerKeyOf(s))
	if err != nil {
		return
	}
	_ = writeFrame(s, body)
}

func (t *Transport) handleCall(s network.Stream) {
	serveReq(s, func(key string, req CallRequest) any { return t.cfg.Handlers.OnCall(context.Background(), key, req) })
}

func (t *Transport) handleGossip(s network.Stream) {
	serveDoc(s, func(key string) (json.RawMessage, error) { return t.cfg.Handlers.OnGossip(context.Background(), key) })
}

func (t *Transport) handleInspect(s network.Stream) {
	serveDoc(s, func(key string) (json.RawMessage, error) { return t.cfg.Handlers.OnInspect(context.Background(), key) })
}

func (t *Transport) handleManifest(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	frames, err := t.cfg.Handlers.OnManifest(context.Background(), peerKeyOf(s))
	if err != nil {
		return
	}
	// Send the count, then one frame per action manifest.
	_ = writeFrame(s, map[string]int{"count": len(frames)})
	for _, f := range frames {
		if err := writeFrame(s, json.RawMessage(f)); err != nil {
			return
		}
	}
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
		return resp, err
	}
	if err := readFrame(s, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// Call sends a federation call to the peer and returns its settlement envelope.
func (t *Transport) Call(ctx context.Context, peerKey string, req CallRequest) (CallResponse, error) {
	return roundTrip[CallRequest, CallResponse](ctx, t, peerKey, ProtocolCall, req)
}

// Gossip fetches the peer's gossip document.
func (t *Transport) Gossip(ctx context.Context, peerKey string) (json.RawMessage, error) {
	return roundTrip[struct{}, json.RawMessage](ctx, t, peerKey, ProtocolGossip, struct{}{})
}

// Inspect fetches the peer's inspect document.
func (t *Transport) Inspect(ctx context.Context, peerKey string) (json.RawMessage, error) {
	return roundTrip[struct{}, json.RawMessage](ctx, t, peerKey, ProtocolInspect, struct{}{})
}

// Manifests fetches the peer's action manifests, one JSON frame per action.
// boundManifestCount rejects a peer-declared manifest chunk count that is negative or past the
// anti-OOM cap, before it is used to size an allocation. A count past the cap is a hostile or
// broken peer, not a real catalog.
func boundManifestCount(n int) error {
	if n < 0 || n > maxManifests {
		return fmt.Errorf("peer declared %d manifests, exceeds max %d", n, maxManifests)
	}
	return nil
}

func (t *Transport) Manifests(ctx context.Context, peerKey string) ([]json.RawMessage, error) {
	s, err := t.openStream(ctx, peerKey, ProtocolManifest)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var head struct {
		Count int `json:"count"`
	}
	if err := readFrame(s, &head); err != nil {
		return nil, err
	}
	// head.Count is peer-controlled; bound it before it sizes an allocation (anti-OOM).
	if err := boundManifestCount(head.Count); err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, head.Count)
	for i := 0; i < head.Count; i++ {
		var f json.RawMessage
		if err := readFrame(s, &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
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
