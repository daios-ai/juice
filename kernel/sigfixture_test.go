package kernel

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// Golden signature fixtures. Every signed federation payload (§12 signature domains, §13 protocols)
// is pinned here as an exact signature string over a fixed key and fixed field values. These digests
// ARE the wire contract: a peer verifies them offline, and the network upgrades in lockstep, so no
// refactor of how a payload is represented in Go may move a single byte. Generated from the payload
// builders before they became typed structs, and never edited since — a failure here means the
// canonical JSON changed, which is a protocol break, not a test to update.
func TestSignedPayloadGoldenFixtures(t *testing.T) {
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

	sig, err := SignFederationPayload(key, actionID, cp, recipient, contract, ikey, ts, argsHash)
	check("fed_call", "tgkkjlOrslDe2bWXhqfWuZ60pkFUFl86Gq8BhpnrzsFAjwheifU1Oi8W1Jj83kRYPm_QOkovsGlLyHOMgkAiAg", sig, err)

	sig, err = SignStepPayload(key, stepID, cp, recipient, ikey, ts, inputHash)
	check("step_complete", "7jYqR305sA1RrHr3nB8zopv19xkzFMIM7-5jlJrZIyBzr6gw003057A8VNJbAnE2u5NWnq8W-eucwQa3ROUUBw", sig, err)

	sig, err = SignStepAuthPayload(key, cp, recipient, userID, stepID, ts)
	check("step_auth", "mERJPFyqpfC3hBehbsp9dV6rQNW-jHdi7uLotLhCq0acCix0QJEoByVzNHfoXP_klmZPkd0B1zFxz7Gt9SRwBQ", sig, err)

	sig, err = SignStepListPayload(key, cp, recipient, ts)
	check("step_list", "1cvubu5dOuQbBkdJRzka8IEtzsZle_AolJZ8mZE0hY-ie8mip-G8xKXd6lDvbArDY8L-rqRDdUfpljOvnAJkBA", sig, err)

	sig, err = signJCS(key, sigDomainSettleOpen, settleOpenPayload(cp, recipient, settleID, 42, ts))
	check("settle_open", "bHpQWWC8QiT-zZKep_7tW_0CgzAtpyD-swxWnF4oDPi9LOz0-ue2HMejiyLYEqQfUxWDc-ZVdpHK-4VcjX7sDQ", sig, err)

	sig, err = signJCS(key, sigDomainSettleFinish, settleFinishPayload(cp, recipient, settleID, nonce, ts))
	check("settle_finish", "Yp1LzUnvUfl7sJeBtGG-qS3O9D4lrRXhfeZYJ3ps2ViN5s0p-67r6EgpZlQIvc8hTbftOogsQOX1W77Jra4NDQ", sig, err)

	sig, err = signJCS(key, sigDomainSettleReconcile, settleReconcilePayload(cp, recipient, settleID, ts))
	check("settle_reconcile", "T8z-n9Wc_pPBRjlJNbfkbcBJI06Ap4vcNphAQOJzHImYul133alf-X9OC3hCu5Z6vwi1Ptb7-C6VB8UzJGq-Dw", sig, err)

	m := &ActionManifest{
		ActionID: actionID, OwnerID: "owner-1", OwnerHandle: "alice", Name: "greet",
		Description: "greets", Price: 10, RemoteBPS: 500, Kind: "wasm", ArtifactHash: "af-1",
		UpdatedAt:   fixedTime,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
	}
	sig, err = SignManifest(key, m)
	check("manifest", "KFLZ2FOnyH1GmAFVuUGp7R1Ks4Bl5iqCkb0VTZKkmKWn38EShXYcVywxk8nwDDbc9DXAZWB5_XmVAI5hY-UVBg", sig, err)

	rec := &SettlementRecord{
		SettlementID: settleID, Creditor: cp, Debtor: recipient, Amount: 42, Quantum: 100,
		Mode: "probabilistic", Commitment: "H", Nonce: nonce, Secret: "s", Outcome: "pay",
		ExpiresAt: fixedTime, CreatedAt: fixedTime,
	}
	sig, err = signJCS(key, sigDomainSettlementRec, rec)
	check("settlement_record", "4AerKb9Yx5FCYG2aHT5VMaDk-eMV8FFqmoXeFwB8vZk3hYDMpZhKWQS2AqAuVGoOLisaFBP5q3vROhd1sxqlDw", sig, err)
}
