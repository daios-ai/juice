package kernel

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// playNetworkDigest is the play world's digest, pinned identically in rail/world_test.go. These
// fixtures are signatures on that network, so the two constants must agree or kernels that believe
// they share a world would reject each other.
const playNetworkDigest = "ef1fac03f5f78ca42dfa05b9eb975b5e0944e013ed1eb5ea30a2be9328e34a67"

// playNet is the network every test signs on, so a fixture and a round-trip agree by construction.
var playNet = Network{Name: "play", Digest: playNetworkDigest}

// Golden signature fixtures. Every signed federation payload (§12 signature domains, §13 protocols)
// is pinned here as an exact signature string over a fixed key and fixed field values. These digests
// ARE the wire contract: a peer verifies them offline, and the network upgrades in lockstep, so no
// refactor of how a payload is represented in Go may move a single byte — and neither may the
// network digest the prefix carries. A failure here is a protocol break, not a test to update.
func TestSignedPayloadGoldenFixtures(t *testing.T) {
	net := playNet
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	key := ed25519.NewKeyFromSeed(seed)

	const (
		cp        = "counterparty-key"
		recipient = "recipient-key"
		ts        = "2026-01-02T03:04:05Z"
		ikey      = "idem-1"
		argsHash  = "args-hash"
		contract  = "contract-hash"
		actionID  = "action-1"
		stepID    = "step-1"
		inputHash = "input-hash"
		userID    = "user-1"
		settleID  = "settle-1"
		nonce     = "nonce-1"
	)
	fixedTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	check := func(name, want string, got string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s signature moved — canonical JSON changed (protocol break):\n got  %s\n want %s", name, got, want)
		}
	}

	// A call that stakes no ticket omits both new fields, so its canonical form — and this
	// signature — are exactly what they were before the lottery existed.
	sig, err := net.SignFederationPayload(key, actionID, cp, recipient, contract, ikey, ts, argsHash, "", 0)
	check("fed_call", "7nvN8-uTEDnPSjjxUvSNBp-pfbUIgtKe3SvSRWv2hPCoELwhjoz_Z3TrRkde8c08qCyGnCy4HbZcUb2V7B-zCw", sig, err)

	sig, err = net.SignFederationPayload(key, actionID, cp, recipient, contract, ikey, ts, argsHash, "cm-1", 100)
	check("fed_call_ticket", "0m14WxVaIc55g7LlT_qXVVaS9bS8dj8LewbnbAdivDoH_3XIjfT_aubuDv1p0yzgZimyXgKjbSrX4UL08yfjBQ", sig, err)

	sig, err = net.SignStepPayload(key, stepID, cp, recipient, ikey, ts, inputHash)
	check("step_complete", "pG89K-ofhZ8xs-ggsVRJ9eNGeOsTcTFQejYfyIboALe7WyHVjZl2qKEF2-Gk4YDCk8vL37QWlIHOwPXHkiueDA", sig, err)

	sig, err = net.SignStepAuthPayload(key, cp, recipient, userID, stepID, ts, false)
	check("step_auth", "qxmzAfhsS8m80NKP_DTapYAyso-wR_zxDYvOQtDqO9LJonZZOp1hlpj6oBycpfopKaitmgnKrsQfndJkYp2VDA", sig, err)

	sig, err = net.SignStepListPayload(key, cp, recipient, ts, "")
	check("step_list", "hWP8ddJWQ4eDzSPL9T55CVoFzKhWOvlgiZpBfZtNQC1Z4BA1nhU0bFxDCXvrdDE0ZN8xUG-9xB0KOLo9vyjnCg", sig, err)

	sig, err = net.sign(key, sigDomainReveal, RevealPayload{
		Counterparty: cp, Recipient: recipient, Secret: "s", TicketID: settleID, Timestamp: ts, TxHash: "tx-1",
	})
	check("reveal", "hjdLt6kUB26CIp1ODDCEtfsjYQUGZ4ymN6duaxbur9BicucTq3cKqNJJlc_DfcehEnfd-oYxEAPMfSD4v5n3CA", sig, err)

	m := &ActionManifest{
		ActionID: actionID, OwnerID: "owner-1", OwnerHandle: "alice", Name: "greet",
		Description: "greets", Price: 10, RemoteBPS: 500, Kind: "wasm", ArtifactHash: "af-1",
		UpdatedAt:   fixedTime,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}
	sig, err = net.SignManifest(key, m)
	check("manifest", "WOYz6ZZKYV4jhDweoekj5wGyTWPbtY-e-Jgpb5RAVJ-cUOUiglPttUo2YrjZK51RhJaKYpknvMIZhUggOBEpDw", sig, err)

}
