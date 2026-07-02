package fed

import (
	"context"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
)

// Reachability describes how this transport can reach a peer right now — the diagnostic the
// operator sees via `admin inspect <key>` now that there is no browser-reachable endpoint.
type Reachability struct {
	Path      string   `json:"path"`       // "direct", "relayed", or "unreachable"
	RTTmillis int64    `json:"rtt_millis"` // round-trip time of the resolve+connect, milliseconds
	Protocols []string `json:"protocols"`  // libp2p protocols the peer advertises
	Error     string   `json:"error,omitempty"`
}

// Probe resolves and connects to a peer by key and reports the reachability path. It does not
// send a federation call — it only establishes (or reuses) a connection and classifies it.
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

var (
	_ = network.Connected
	_ = isRelayAddr
)
