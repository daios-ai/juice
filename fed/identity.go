package fed

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The federation network identity is derived deterministically from the platform Ed25519
// signing key — there is no second key (§12). A peer's base64url Ed25519 public key is
// simultaneously its Juice identity (§13) and its address on this transport.

// deriveHostKey converts a standard library ed25519 private key into a libp2p private key.
// The mapping is deterministic and 1:1, so the libp2p peer ID is a pure function of the
// platform signing key.
func deriveHostKey(priv ed25519.PrivateKey) (libp2pcrypto.PrivKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("fed: signing key must be a %d-byte ed25519 private key, got %d", ed25519.PrivateKeySize, len(priv))
	}
	return libp2pcrypto.UnmarshalEd25519PrivateKey(priv)
}

// PeerIDFromKey maps a base64url Ed25519 public key (the identity used everywhere in §13)
// to the libp2p peer ID the transport dials. Deterministic and 1:1.
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

// KeyFromPeerID recovers the base64url Ed25519 public key from a libp2p peer ID. Ed25519
// peer IDs embed the public key inline, so this never needs the network.
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
