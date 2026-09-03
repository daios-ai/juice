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

	sig, err := net.SignFederationPayload(key, actionID, cp, recipient, contract, ikey, ts, argsHash)
	check("fed_call", "7nvN8-uTEDnPSjjxUvSNBp-pfbUIgtKe3SvSRWv2hPCoELwhjoz_Z3TrRkde8c08qCyGnCy4HbZcUb2V7B-zCw", sig, err)

	sig, err = net.SignStepPayload(key, stepID, cp, recipient, ikey, ts, inputHash)
	check("step_complete", "pG89K-ofhZ8xs-ggsVRJ9eNGeOsTcTFQejYfyIboALe7WyHVjZl2qKEF2-Gk4YDCk8vL37QWlIHOwPXHkiueDA", sig, err)

	sig, err = net.SignStepAuthPayload(key, cp, recipient, userID, stepID, ts)
	check("step_auth", "qxmzAfhsS8m80NKP_DTapYAyso-wR_zxDYvOQtDqO9LJonZZOp1hlpj6oBycpfopKaitmgnKrsQfndJkYp2VDA", sig, err)

	sig, err = net.SignStepListPayload(key, cp, recipient, ts)
	check("step_list", "hWP8ddJWQ4eDzSPL9T55CVoFzKhWOvlgiZpBfZtNQC1Z4BA1nhU0bFxDCXvrdDE0ZN8xUG-9xB0KOLo9vyjnCg", sig, err)

	sig, err = net.sign(key, sigDomainSettleOpen, settleOpenPayload(cp, recipient, settleID, 42, ts))
	check("settle_open", "pw7IjPlg2dO4N6zDTPfdpV8aRiNzthHy7e9TXlERHC6WZW0yYqm3hIi-wxrfi6QkhjOttbgYCqaFIfCTqPWuDg", sig, err)

	sig, err = net.sign(key, sigDomainSettleFinish, settleFinishPayload(cp, recipient, settleID, nonce, ts))
	check("settle_finish", "FL4cBKM0uSLirTLoEVSGo55Yto5CnHkWZujVL9r_2cScomtl_J6WVgXay-UvQ0vNjnWjfp8-X0t0WB95ctU-Dg", sig, err)

	sig, err = net.sign(key, sigDomainSettleAnnounce, settleAnnouncePayload(cp, recipient, settleID, "tx-1", ts))
	check("settle_announce", "s-xO3HCxXhZLhGQuX_lAa6djf9AGtVuj7cP--pViJkRpcS8Ss52t_wfx0BbIKqKTtubKFNhpcJCglhzf7ZT7CQ", sig, err)

	sig, err = net.sign(key, sigDomainSettleReconcile, settleReconcilePayload(cp, recipient, settleID, ts))
	check("settle_reconcile", "YtLuLELLzN69HTiLQi6OaOXJADKHk6rth9Ly9r9n5aGtwkWnTHADveN6vLRuaiV-0NKwC2cdVvnKqxAOpGS_Dw", sig, err)

	m := &ActionManifest{
		ActionID: actionID, OwnerID: "owner-1", OwnerHandle: "alice", Name: "greet",
		Description: "greets", Price: 10, RemoteBPS: 500, Kind: "wasm", ArtifactHash: "af-1",
		UpdatedAt:   fixedTime,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}
	sig, err = net.SignManifest(key, m)
	check("manifest", "WOYz6ZZKYV4jhDweoekj5wGyTWPbtY-e-Jgpb5RAVJ-cUOUiglPttUo2YrjZK51RhJaKYpknvMIZhUggOBEpDw", sig, err)

	rec := &SettlementRecord{
		SettlementID: settleID, Creditor: cp, Debtor: recipient, Amount: 42, Quantum: 100,
		Mode: "probabilistic", Commitment: "H", Nonce: nonce, Secret: "s", Outcome: "pay",
		ExpiresAt: fixedTime, CreatedAt: fixedTime,
	}
	sig, err = net.sign(key, sigDomainSettlementRec, rec)
	check("settlement_record", "5le2X3fMYhyPPZvbERmeaqNABEwr5qBNq5-MvqLzW9u_EyKc4wH0mXJX_ZArQvDdaxYMEisiEyVYjoKYWqIdAg", sig, err)
}
