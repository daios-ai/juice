package fed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// Wire framing: each message is a 4-byte big-endian length prefix followed by that many JSON
// bytes. Manifest sync sends one frame per action so a large catalog survives a bandwidth-
// capped relayed connection; every other protocol is a single request frame and response frame.

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
	t.host.SetStreamHandler(protocol.ID(ProtocolFriend), t.handleFriend)
	t.host.SetStreamHandler(protocol.ID(ProtocolManifest), t.handleManifest)
	t.host.SetStreamHandler(protocol.ID(ProtocolGossip), t.handleGossip)
	t.host.SetStreamHandler(protocol.ID(ProtocolInspect), t.handleInspect)
}

func (t *Transport) handleCall(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	var req CallRequest
	if err := readFrame(s, &req); err != nil {
		return
	}
	resp := t.cfg.Handlers.OnCall(context.Background(), peerKeyOf(s), req)
	_ = writeFrame(s, resp)
}

func (t *Transport) handleFriend(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	var req FriendRequest
	if err := readFrame(s, &req); err != nil {
		return
	}
	resp := t.cfg.Handlers.OnFriend(context.Background(), peerKeyOf(s), req)
	_ = writeFrame(s, resp)
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

func (t *Transport) handleGossip(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	body, err := t.cfg.Handlers.OnGossip(context.Background(), peerKeyOf(s))
	if err != nil {
		return
	}
	_ = writeFrame(s, json.RawMessage(body))
}

func (t *Transport) handleInspect(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(streamDeadline))
	body, err := t.cfg.Handlers.OnInspect(context.Background(), peerKeyOf(s))
	if err != nil {
		return
	}
	_ = writeFrame(s, json.RawMessage(body))
}

// ---- outbound client ----

func (t *Transport) openStream(ctx context.Context, peerKey, proto string) (network.Stream, error) {
	pid, err := t.resolve(ctx, peerKey)
	if err != nil {
		return nil, err
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

// Call sends a federation call to the peer and returns its settlement envelope.
func (t *Transport) Call(ctx context.Context, peerKey string, req CallRequest) (CallResponse, error) {
	s, err := t.openStream(ctx, peerKey, ProtocolCall)
	if err != nil {
		return CallResponse{}, err
	}
	defer s.Close()
	if err := writeFrame(s, req); err != nil {
		return CallResponse{}, err
	}
	var resp CallResponse
	if err := readFrame(s, &resp); err != nil {
		return CallResponse{}, err
	}
	return resp, nil
}

// Friend sends a friend handshake to the peer.
func (t *Transport) Friend(ctx context.Context, peerKey string, req FriendRequest) (FriendResponse, error) {
	s, err := t.openStream(ctx, peerKey, ProtocolFriend)
	if err != nil {
		return FriendResponse{}, err
	}
	defer s.Close()
	if err := writeFrame(s, req); err != nil {
		return FriendResponse{}, err
	}
	var resp FriendResponse
	if err := readFrame(s, &resp); err != nil {
		return FriendResponse{}, err
	}
	return resp, nil
}

// Manifests fetches the peer's action manifests, one JSON frame per action.
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

// Gossip fetches the peer's gossip document.
func (t *Transport) Gossip(ctx context.Context, peerKey string) (json.RawMessage, error) {
	return t.fetchOne(ctx, peerKey, ProtocolGossip)
}

// Inspect fetches the peer's inspect document.
func (t *Transport) Inspect(ctx context.Context, peerKey string) (json.RawMessage, error) {
	return t.fetchOne(ctx, peerKey, ProtocolInspect)
}

func (t *Transport) fetchOne(ctx context.Context, peerKey, proto string) (json.RawMessage, error) {
	s, err := t.openStream(ctx, peerKey, proto)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	// Request frame is empty; the server replies with the document.
	if err := writeFrame(s, struct{}{}); err != nil {
		return nil, err
	}
	var body json.RawMessage
	if err := readFrame(s, &body); err != nil {
		return nil, err
	}
	return body, nil
}

var _ = peer.ID("")
