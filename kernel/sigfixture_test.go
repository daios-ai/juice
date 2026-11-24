// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
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
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	key := KeyFromSeed(seed)

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

	// The key itself is part of the contract: how a kernel is named in every payload (P1).
	if got, want := PublicKey(key), "AoS_dWImK71pQAhXSPO-avpSrjFxVRgezjG2Y1HM_6Sw"; got != want {
		t.Errorf("the key renders as %s, want %s", got, want)
	}

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
	check("fed_call", "Q4dRcb0v0DS59IInWzLmQna_1jBg5Lic0XTKwJl9pjpJ4g00SEGlu4xj0LCg6gTlc0chxGhquCTsJiN_XZs6qA", sig, err)

	call.Commitment, call.Lottery = "cm-1", 100
	sig, err = net.SignFederationPayload(key, call, cp, recipient, ts, argsHash)
	check("fed_call_ticket", "Z-kVR46r9rZUePZX1x5mSgMuf6c5GpTxwtOwkBDgH6JbD8bdCslJZ_yl89HE58non811LPpwwRy8tpxV9gKT0g", sig, err)

	// The caller the buyer attests rides in the same payload (P4); a call for no user of the
	// buyer's omits both fields, so the two forms above are what they were.
	call.CallerUserID, call.CallerHandle = userID, "alice"
	sig, err = net.SignFederationPayload(key, call, cp, recipient, ts, argsHash)
	check("fed_call_caller", "Ey-Tpz9pYHNMJVIPLL1FDdhLKQ2lVxxCUKH_k_RkEEsyvQB9-2vSigaXOVtvOoLT57XHu878AD33mHcphZjpLA", sig, err)

	sig, err = net.SignTaskPayload(key, "complete", taskID, cp, recipient, ikey, ts, inputHash, "", false)
	check("task_complete", "_pZH5HNzNaPNmWrj34llicHh8cO-8wWUXSxyiWykMNtFqToaVJOdYEh5bzuzIWuUrOjmLWmizCkj3H9c9XNCsA", sig, err)

	// A completion as a user signs the user into the same payload (P8), superuser or not.
	sig, err = net.SignTaskPayload(key, "complete", taskID, cp, recipient, ikey, ts, inputHash, userID, false)
	check("task_complete_user", "Cf1mHiMmvueoiEcGxi8UDOUkS9ueJueyuxil5wuOH1Z9SuenYgtloloTPHYwSbA8zvXV72fX-cjtw3yNkdAZjQ", sig, err)
	sig, err = net.SignTaskPayload(key, "complete", taskID, cp, recipient, ikey, ts, inputHash, userID, true)
	check("task_complete_operator", "EERyDj7PDQ8UE81fUVGCCBj99fB8BcUaq9O6iATbRuExxrONFfwg9JQAvUHYFpFJrFHya_0YfVokBCkt2bZ-pQ", sig, err)

	// A decline is a completion without input, in its own domain (P8).
	sig, err = net.SignTaskPayload(key, "cancel", taskID, cp, recipient, "", ts, "", userID, false)
	check("task_cancel", "2p5vX1koJkEmM39EJiSSyISxx0osb2ipc0Q4H2GpZU4ygNJKmJKmcxLFgHgAZtgcVkQ1T4qJJDB2cnNvVGNBFg", sig, err)

	sig, err = net.sign(key, sigDomainTaskNotice, taskNoticePayload{Counterparty: cp, Recipient: recipient, Timestamp: ts,
		Notice: TaskNotice{ID: taskID, Revision: 2, Status: TaskWaiting, UserID: userID, PartialArgs: []byte(`{"message":"hi"}`),
			AllowedInput: map[string]any{"type": "object"}, Price: 10, CreatedAt: fixedTime}})
	check("task_notice", "Y4k1tVpgrDP0ELdnVlwMmaT0z5AM-pHLWjGBnkl5i4MVToBqfBSjn8R_EtmoVnQ-b381RhNLReMlKpbCJcInDg", sig, err)

	// The completion key is derived, and every stored completion is found again by it (P8): its
	// derivation string is frozen, whatever the protocol is called.
	check("completion_key", "e95133e9c98b1993297cae98ba19229104171f941ff65f9f8d17fb9b9b74ac51",
		TaskIdempotencyKey(recipient, taskID, inputHash), nil)

	sig, err = net.sign(key, sigDomainReveal, RevealPayload{
		Counterparty: cp, Recipient: recipient, Secret: "s", TicketID: settleID, Timestamp: ts, TxHash: "tx-1",
	})
	check("reveal", "lKDPIJcyIpdy-P3-NIZw7gHLs02lFsHTdlSQ_uHXvFoMoAmt6inq64-7sZ2sHLeHn6-Yv4a_4LdmIluaRnYrgg", sig, err)

	m := &ActionManifest{
		ActionID: actionID, OwnerID: "owner-1", OwnerHandle: "alice", Title: "Greet a caller", Name: "greet",
		Description: "greets", Price: 10, RemoteBPS: 500, Kind: "wasm", ArtifactHash: "af-1",
		UpdatedAt:   fixedTime,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}
	sig, err = net.SignManifest(key, m)
	check("manifest", "wl6HcKGQAaLHs8Ww4ydDC2nRouI25OpNrM02k5waPcd5uE03dsZYHgp0tczuu1YJmDkRKMVsAVAjoQXBAZGa0g", sig, err)

}
