package main

// Tests for the outbound federation adapter (§13): envelope parsing, never-dispatched
// classification, and the transport fake that stands in for libp2p.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/daios-ai/juice/fed"
)

// fakeFedCaller stands in for the libp2p transport: it returns a canned CallResponse so the
// envelope-parsing + result/receipt extraction in executeFederationOverTransport is testable
// without a network.
type fakeFedCaller struct {
	resolveResp fed.ResolveResponse
	settleResp  fed.SettleResponse
	resp        fed.CallResponse
	stepResp    fed.StepResponse
	err         error
	lastReq     fed.CallRequest
	lastStep    fed.StepRequest
}

func (f *fakeFedCaller) Call(_ context.Context, _ string, req fed.CallRequest) (fed.CallResponse, error) {
	f.lastReq = req
	return f.resp, f.err
}

func (f *fakeFedCaller) Resolve(_ context.Context, _ string, _ fed.ResolveRequest) (fed.ResolveResponse, error) {
	return f.resolveResp, f.err
}

func (f *fakeFedCaller) Settle(_ context.Context, _ string, _ fed.SettleRequest) (fed.SettleResponse, error) {
	return f.settleResp, f.err
}

func (f *fakeFedCaller) Step(_ context.Context, _ string, req fed.StepRequest) (fed.StepResponse, error) {
	f.lastStep = req
	return f.stepResp, f.err
}

func TestExecuteFederationSuccess(t *testing.T) {
	fc := &fakeFedCaller{resp: fed.CallResponse{
		Status: 200,
		Body:   []byte(`{"result":{"ok":true},"receipt":{"id":"r1","status":"success"}}`),
	}}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := func(action, cp, recipient, chash, ikey, argsHash string) (string, string, error) {
		return "sig", "ts", nil
	}
	fr, err := executeFederationOverTransport(context.Background(), fc, signer,
		base64.RawURLEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		"peerkey", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-123", map[string]any{})
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
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-x", map[string]any{})
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
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-y", map[string]any{})
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
	fr, err := executeFederationOverTransport(context.Background(), fc, nil, "local", "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-z", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fr.NotDispatched {
		t.Errorf("ErrNotDispatched should set NotDispatched, got %+v", fr)
	}

	// nil transport → provably never sent.
	e := &fedAdapter{}
	fr2, err := e.ExecuteFederation(context.Background(), "peer", "3f1c9a2e-0b64-4f7a-9c15-2d8e6b0a7f31", "chash", "key-w", map[string]any{})
	if err != nil {
		t.Fatalf("nil-transport ExecuteFederation: %v", err)
	}
	if !fr2.NotDispatched {
		t.Errorf("nil transport should set NotDispatched, got %+v", fr2)
	}
}
