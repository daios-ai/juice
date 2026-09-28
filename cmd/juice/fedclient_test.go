// SPDX-License-Identifier: AGPL-3.0-only

package main

// Tests for the outbound federation adapter (§13): envelope parsing, never-dispatched
// classification, and the transport fake that stands in for libp2p.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
)

// fakeFedCaller stands in for the libp2p transport: it returns a canned CallResponse so the
// envelope-parsing + result/receipt extraction in executeFederationOverTransport is testable
// without a network.
type fakeFedCaller struct {
	resolveResp fed.ResolveResponse
	revealResp  fed.RevealResponse
	resp        fed.CallResponse
	taskResp    fed.TaskResponse
	err         error
	lastReq     fed.CallRequest
	lastTask    fed.TaskRequest
	// transferResp is the canned answer to an announced transfer, lastTransfer what was announced.
	transferResp fed.TransferResponse
	lastTransfer fed.TransferRequest
}

func (f *fakeFedCaller) Call(_ context.Context, _ string, req fed.CallRequest) (fed.CallResponse, error) {
	f.lastReq = req
	return f.resp, f.err
}

func (f *fakeFedCaller) Resolve(_ context.Context, _ string, _ fed.ResolveRequest) (fed.ResolveResponse, error) {
	return f.resolveResp, f.err
}

func (f *fakeFedCaller) Reveal(_ context.Context, _ string, _ fed.RevealRequest) (fed.RevealResponse, error) {
	return f.revealResp, f.err
}

func (f *fakeFedCaller) Task(_ context.Context, _ string, req fed.TaskRequest) (fed.TaskResponse, error) {
	f.lastTask = req
	return f.taskResp, f.err
}

func (f *fakeFedCaller) Transfer(_ context.Context, _ string, req fed.TransferRequest) (fed.TransferResponse, error) {
	f.lastTransfer = req
	return f.transferResp, f.err
}

func TestExecuteFederationSuccess(t *testing.T) {
	fc := &fakeFedCaller{resp: fed.CallResponse{
		Status: 200,
		Body:   []byte(`{"result":{"ok":true},"receipt":{"id":"r1","status":"success"}}`),
	}}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := func(_ kernel.OutboundCall, _, _, _ string) (string, string, error) {
		return "sig", "ts", nil
	}
	fr, err := executeFederationOverTransport(context.Background(), fc, signer,
		base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		"peerkey", kernel.OutboundCall{ActionID: "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", ExpectedContractHash: "chash", IdempotencyKey: "key-123"}, "", "", map[string]any{})
	if err != nil {
		t.Fatalf("executeFederationOverTransport: %v", err)
	}
	if fr.Result["ok"] != true {
		t.Errorf("result: got %v, want ok:true", fr.Result)
	}
	if fr.ReceiptJSON == "" {
		t.Error("expected non-empty receiptJSON")
	}
	// The exact args bytes were signed and forwarded (the args_hash contract).
	if fc.lastReq.IdempotencyKey != "key-123" || string(fc.lastReq.Args) != "{}" {
		t.Errorf("request not forwarded verbatim: %+v", fc.lastReq)
	}
}

func TestExecuteFederationNon200(t *testing.T) {
	// A non-200 with no parseable receipt → no receipt, status propagated (caller stays pending).
	fc := &fakeFedCaller{resp: fed.CallResponse{Status: 500, Body: []byte(`error`)}}
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", kernel.OutboundCall{ActionID: "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", ExpectedContractHash: "chash", IdempotencyKey: "key-x"}, "", "", map[string]any{})
	if err != nil {
		t.Fatalf("executeFederationOverTransport: unexpected error: %v", err)
	}
	if fr.ReceiptJSON != "" {
		t.Error("expected empty receiptJSON for non-receipt response")
	}
	if fr.HTTPStatus != 500 {
		t.Errorf("expected HTTPStatus=500, got %d", fr.HTTPStatus)
	}
}

// A transport error yields a zero result so the kernel keeps the call pending for retry (§13).
// A plain (post-connect) error is NOT NotDispatched: the request may have executed remotely.
func TestExecuteFederationTransportError(t *testing.T) {
	fc := &fakeFedCaller{err: fmt.Errorf("unreachable")}
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", kernel.OutboundCall{ActionID: "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", ExpectedContractHash: "chash", IdempotencyKey: "key-y"}, "", "", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fr.HTTPStatus != 0 || fr.ReceiptJSON != "" || fr.NotDispatched {
		t.Errorf("plain transport error should be pending (not NotDispatched), got %+v", fr)
	}
}

// A never-dispatched transport error (resolve/connect failed) sets NotDispatched so a first
// dispatch can fail fast (§13). The nil-transport executor is the same provably-never-sent case.
func TestExecuteFederationNotDispatched(t *testing.T) {
	fc := &fakeFedCaller{err: fmt.Errorf("%w: cannot resolve", fed.ErrNotDispatched)}
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", kernel.OutboundCall{ActionID: "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", ExpectedContractHash: "chash", IdempotencyKey: "key-z"}, "", "", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fr.NotDispatched {
		t.Errorf("ErrNotDispatched should set NotDispatched, got %+v", fr)
	}

	// nil transport → provably never sent.
	e := &fedAdapter{}
	fr2, err := e.ExecuteFederation(context.Background(), "peer", kernel.OutboundCall{ActionID: "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", ExpectedContractHash: "chash", IdempotencyKey: "key-w"}, map[string]any{})
	if err != nil {
		t.Fatalf("nil-transport ExecuteFederation: %v", err)
	}
	if !fr2.NotDispatched {
		t.Errorf("nil transport should set NotDispatched, got %+v", fr2)
	}
}

// One rule decides reachability everywhere (§13). The bug this pins: an error-returning path and
// the call path once disagreed about the SAME network event — a stream that broke after dispatch —
// so a lost connection advanced last_seen on one and last_contact_failed_at on the other. Only a
// reply proves the peer was reached and only an undispatched request proves it was not; everything
// between proves nothing, whichever operation produced it.
func TestContactClassificationAgreesAcrossPaths(t *testing.T) {
	notDispatched := fmt.Errorf("%w: cannot resolve peer", fed.ErrNotDispatched)
	midStream := fmt.Errorf("fed: read call: stream reset")

	for _, tc := range []struct {
		name       string
		fromErr    error
		fromResult kernel.FederationResult
		want       contactOutcome
	}{
		{"answered", nil, kernel.FederationResult{HTTPStatus: 200}, contactReached},
		{"answered with a refusal", nil, kernel.FederationResult{HTTPStatus: 402}, contactReached},
		{"never dispatched", notDispatched, kernel.FederationResult{NotDispatched: true}, contactUndispatched},
		{"broke after dispatch", midStream, kernel.FederationResult{}, contactUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contactFromErr(tc.fromErr); got != tc.want {
				t.Errorf("contactFromErr = %v, want %v", got, tc.want)
			}
			if got := contactFromResult(tc.fromResult); got != tc.want {
				t.Errorf("contactFromResult = %v, want %v — the two paths must agree", got, tc.want)
			}
		})
	}
}

// The call a kernel decides to make and the request it sends are two shapes joined by one mapping.
// Every wire field must carry its own source, so a field dropped or swapped fails here, and the
// receiver must verify the request once it rebuilds the call from the wire, as OnCall does.
func TestAnOutboundCallReachesTheWireWhole(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	local := base64.RawURLEncoding.EncodeToString(pub)
	call := kernel.OutboundCall{ActionID: "v-action", ExpectedContractHash: "v-hash", IdempotencyKey: "v-key",
		Commitment: "v-commitment", Lottery: 7, CallerUserID: "v-user", CallerHandle: "v-handle"}
	fc := &fakeFedCaller{resp: fed.CallResponse{Status: 200, Body: []byte(`{"result":{},"receipt":{"id":"r"}}`)}}
	signer := func(c kernel.OutboundCall, counterparty, recipient, argsHash string) (string, string, error) {
		sig, err := testNet.SignFederationPayload(priv, c, counterparty, recipient, "2026-09-28T12:00:00Z", argsHash)
		return sig, "2026-09-28T12:00:00Z", err
	}
	if _, err := executeFederationOverTransport(context.Background(), fc, signer, local, "peerkey",
		call, "v-address", "v-proof", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	req := fc.lastReq
	got := kernel.OutboundCall{ActionID: req.Action, ExpectedContractHash: req.ExpectedContractHash,
		IdempotencyKey: req.IdempotencyKey, Commitment: req.Commitment, Lottery: req.Lottery,
		CallerUserID: req.CallerUserID, CallerHandle: req.CallerHandle}
	if got != call || req.Counterparty != local || req.BlockchainAddress != "v-address" || req.BlockchainProof != "v-proof" || string(req.Args) != `{"n":1}` {
		t.Errorf("wire request = %+v, want every field from its own source", req)
	}
	rv := reflect.ValueOf(req)
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).IsZero() {
			t.Errorf("CallRequest.%s was not set by the mapping", rv.Type().Field(i).Name)
		}
	}
	sum := sha256.Sum256(req.Args)
	if err := testNet.VerifyFederationSignature(req.Counterparty, got, req.Counterparty, "peerkey", req.Timestamp,
		hex.EncodeToString(sum[:]), req.Signature); err != nil {
		t.Errorf("the receiver cannot verify what was sent: %v", err)
	}
}
