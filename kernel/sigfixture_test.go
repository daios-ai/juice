// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// playNetworkFingerprint is the play world's fingerprint, pinned identically in rail/world_test.go. These
// fixtures are signatures on that network, so the two constants must agree or kernels that believe
// they share a world would reject each other.
const playNetworkFingerprint = "ef1fac03f5f78ca42dfa05b9eb975b5e0944e013ed1eb5ea30a2be9328e34a67"

// playNet is the network every test signs on, so a fixture and a round-trip agree by construction.
var playNet = Network{Name: "play", Fingerprint: playNetworkFingerprint}

// Golden signature fixtures. Every signed federation payload (§12 signature domains, §13 protocols)
// is pinned here as an exact signature string over a fixed key and fixed field values. These digests
// ARE the wire contract: a peer verifies them offline, and the network upgrades in lockstep, so no
// refactor of how a payload is represented in Go may move a single byte — and neither may the
// network fingerprint the prefix carries. A failure here is a protocol break, not a test to update.
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
		taskID    = "step-1"
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
	call := OutboundCall{ActionID: actionID, ExpectedContractHash: contract, IdempotencyKey: ikey}
	sig, err := net.SignFederationPayload(key, call, cp, recipient, ts, argsHash)
	check("fed_call", "7nvN8-uTEDnPSjjxUvSNBp-pfbUIgtKe3SvSRWv2hPCoELwhjoz_Z3TrRkde8c08qCyGnCy4HbZcUb2V7B-zCw", sig, err)

	call.Commitment, call.Lottery = "cm-1", 100
	sig, err = net.SignFederationPayload(key, call, cp, recipient, ts, argsHash)
	check("fed_call_ticket", "0m14WxVaIc55g7LlT_qXVVaS9bS8dj8LewbnbAdivDoH_3XIjfT_aubuDv1p0yzgZimyXgKjbSrX4UL08yfjBQ", sig, err)

	// The caller the buyer attests rides in the same payload (P4); a call for no user of the
	// buyer's omits both fields, so the two forms above are what they were.
	call.CallerUserID, call.CallerHandle = userID, "alice"
	sig, err = net.SignFederationPayload(key, call, cp, recipient, ts, argsHash)
	check("fed_call_caller", "vLBwQW1n2e916IxZ8diC0HF5QEPjDiKg4YqPGolxOps-TLcsSoC7HLzZPfNz6eN0SNi2oYSNFkFQFE4fQJybAQ", sig, err)

	sig, err = net.SignTaskPayload(key, "complete", taskID, cp, recipient, ikey, ts, inputHash, "", false)
	check("task_complete", "pQgMwIylyms1yyzW95MqbooLqNe7VtlzVLIQnSs2JIgjRrBN2iyYNwzgFqfdIHtgJtNoK4aCk4B2CeuommkNDg", sig, err)

	// A completion as a user signs the user into the same payload (P8), superuser or not.
	sig, err = net.SignTaskPayload(key, "complete", taskID, cp, recipient, ikey, ts, inputHash, userID, false)
	check("task_complete_user", "VaTWccPhR88m1r1NXbXjG2fjPckeAjdWoFMa7uzcUisy0SLOKh0qKRir-6X70z2_jTK89lItwo3gqKQsmS8LCw", sig, err)
	sig, err = net.SignTaskPayload(key, "complete", taskID, cp, recipient, ikey, ts, inputHash, userID, true)
	check("task_complete_operator", "dSk8b4X90DqGB9n2z3bT5uuGGe94B_Y--aw2ERg36qlWBPIioLLK-JhW6ovjz-Xp_bGYQTXI7MKuzrKP7SIcCw", sig, err)

	// A decline is a completion without input, in its own domain (P8).
	sig, err = net.SignTaskPayload(key, "cancel", taskID, cp, recipient, "", ts, "", userID, false)
	check("task_cancel", "PisQ2rh8G6awtrg33arenLFDQrX_3toEDRcNfF4nBWPGSMkg6FXWTrdSvOhLbLJ1YKv7s1lB9wcLyGAjbukRDw", sig, err)

	sig, err = net.sign(key, sigDomainTaskNotice, taskNoticePayload{Counterparty: cp, Recipient: recipient, Timestamp: ts,
		Notice: TaskNotice{ID: taskID, Revision: 2, Status: TaskWaiting, UserID: userID, PartialArgs: []byte(`{"message":"hi"}`),
			AllowedInput: map[string]any{"type": "object"}, Price: 10, CreatedAt: fixedTime}})
	check("task_notice", "3ErE3GKK8vh9XUmeScYrjpGtTlLvoKYyuaHKDjuAwxjKg0rEb_72lOgd-6UQMnOkDZQocikPothpV0-1lhhZAw", sig, err)

	// The completion key is derived, and every stored completion is found again by it (P8): its
	// derivation string is frozen, whatever the protocol is called.
	check("completion_key", "e95133e9c98b1993297cae98ba19229104171f941ff65f9f8d17fb9b9b74ac51",
		TaskIdempotencyKey(recipient, taskID, inputHash), nil)

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
	check("manifest", "7k-HbWrpAUquGLUS0WxsXObXe_KfFuxSSwXHaXlma83pN4bROkK4n0SvDfLnUnmdRjuU_UNdDKcUXPj4Hs5iBw", sig, err)

}
