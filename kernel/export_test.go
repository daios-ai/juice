// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
)

// Test-only seam (the standard export_test.go idiom): the call engine and its request are private,
// because most orchestration modes — a pre-funded trace, a step id, an inbound idempotency record —
// are only valid when the kernel itself supplies them (§4). The package's external tests still need
// to drive those modes directly, so they reach them here rather than through an exported surface a
// client could misuse.
type TestCallRequest struct {
	CallerID        string
	ParentTraceID   string
	Action          *Action
	ActionRef       string
	Args            map[string]any
	StepID          string
	ExistingTraceID string
	// TargetUserID + ActionName name the action by owner id and stored name, a test convenience
	// that reads the row directly: production names an action by address or id alone.
	TargetUserID string
	ActionName   string
}

// TestCall drives the private engine with any orchestration mode.
func (k *Kernel) TestCall(ctx context.Context, req TestCallRequest) (*CallReply, error) {
	if req.TargetUserID != "" && req.Action == nil && req.ActionRef == "" {
		// The owner's address on this kernel, so the engine resolves — and refuses — in its own order.
		owner, err := k.resolveUser(ctx, req.TargetUserID)
		if err != nil || owner == nil {
			return nil, ErrNotFound.Wrap("target user not found")
		}
		if owner.Handle != "" {
			req.ActionRef = Address{Handle: owner.Handle, Kernel: TestOwnName, Name: req.ActionName}.String()
		} else {
			// A proxy is owned by a peer's account: its address is the remote owner's, beneath the peer.
			ro, rest := SplitProxyName(req.ActionName)
			req.ActionRef = Address{Handle: ro, Kernel: owner.KernelPublicKey, Name: rest}.String()
		}
	}
	return k.call(ctx, callRequest{CallerID: req.CallerID, ParentTraceID: req.ParentTraceID, Action: req.Action,
		ActionRef: req.ActionRef, Args: req.Args, StepID: req.StepID, ExistingTraceID: req.ExistingTraceID})
}

// DispatchRecordForTest builds the record beginRun freezes on a dispatched trace: the rates, the
// secret and the lottery this call is committed to. A test that stages a proxy trace by hand needs
// it, because settlement reads every pricing input from here and nowhere else (§13).
func DispatchRecordForTest(mp, gross, remoteBPS, importBPS, lottery int64, secret string) *string {
	return marshalDispatch(nil, "", mp, gross, "", remoteBPS, importBPS, secret, lottery, Principal{})
}

// PublicKeyB64 is this kernel's own key in the form evidence names a subject by.
func (k *Kernel) PublicKeyB64() string { return k.ourKeyB64() }

// ServingRecordForTest is the seller's half of the same record: what a foreign call was admitted
// under, for a test that stages one by hand.
func ServingRecordForTest(remoteBPS, lottery, reserve int64, nonce, commitment string) *string {
	return marshalServing(remoteBPS, lottery, reserve, nonce, commitment)
}

// CatalogPageSizeForTest is the page size this protocol serves, so a test about the bound reads it
// from the bound itself rather than restating the number.
const CatalogPageSizeForTest = catalogPageSize

// TestOwnName is the name every test kernel calls itself, so a test address reads `alice@k`.
const TestOwnName = "k"

// SelfKeyForTest is the key this kernel knows itself by, as ResolveKernel reads it.
func (k *Kernel) SelfKeyForTest(ctx context.Context) string { return k.selfKey(ctx) }

// BindOwnNameForTest gives a test kernel its own name the way boot does (D15), under a fixed test
// key when no signing key is configured yet, so ResolveKernel knows "this kernel" from the start.
func (k *Kernel) BindOwnNameForTest(ctx context.Context) {
	if k.selfKey(ctx) == "" {
		_ = k.store.SetConfig(ctx, "signing_public_key", base64.RawURLEncoding.EncodeToString(testOwnKey.Public().(ed25519.PublicKey)))
	}
	if err := k.BindOwnName(ctx, TestOwnName); err != nil {
		panic(err)
	}
}

var testOwnKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
