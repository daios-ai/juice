// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"time"

	ethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// Test-only seam (the standard export_test.go idiom): the call engine and its request are private,
// because most orchestration modes — a pre-funded trace, a task id, an inbound idempotency record —
// are only valid when the kernel itself supplies them (§4). The package's external tests still need
// to drive those modes directly, so they reach them here rather than through an exported surface a
// client could misuse.
type TestCallRequest struct {
	CallerID        string // a user of this kernel; CallerKernel set makes it a user of that peer
	CallerKernel    string
	ParentTraceID   string
	Action          *Action
	ActionRef       string
	Args            map[string]any
	TaskID          string
	ExistingTraceID string
	// IdempotencyRecordID is the inbound record the call answers, as a federated run threads it.
	IdempotencyRecordID string
	// TargetUserID + ActionName name the action by owner id and stored name, a test convenience
	// that reads the row directly: production names an action by address or id alone.
	TargetUserID string
	ActionName   string
}

// TestCall drives the private engine with any orchestration mode.
func (k *Kernel) TestCall(ctx context.Context, req TestCallRequest) (*CallReply, error) {
	if req.TargetUserID != "" && req.Action == nil && req.ActionRef == "" {
		// The owner's address on this kernel, so the engine resolves — and refuses — in its own order.
		owner, err := k.store.ReadUser(ctx, req.TargetUserID)
		if err != nil || owner == nil {
			return nil, ErrNotFound.Wrap("target user not found")
		}
		req.ActionRef = Address{User: owner.Handle, Kernel: TestOwnName, Name: req.ActionName}.String()
	}
	caller := Principal{Kernel: req.CallerKernel, UserID: req.CallerID}
	return k.call(ctx, callRequest{Caller: caller, ParentTraceID: req.ParentTraceID, Action: req.Action,
		ActionRef: req.ActionRef, Args: req.Args, TaskID: req.TaskID, ExistingTraceID: req.ExistingTraceID,
		IdempotencyRecordID: req.IdempotencyRecordID})
}

// DispatchRecordForTest builds the record beginRun freezes on a dispatched trace: the rates, the
// secret and the lottery this call is committed to. A test that stages a proxy trace by hand needs
// it, because settlement reads every pricing input from here and nowhere else (§13).
func DispatchRecordForTest(mp, gross, remoteBPS, importBPS, lottery int64, secret string) *string {
	return marshalDispatch(nil, "", mp, gross, "", remoteBPS, importBPS, secret, lottery, Principal{})
}

// OwnAddress is this kernel's own address, the form evidence names a subject by.
func (k *Kernel) OwnAddress() string { return k.ourAddress() }

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

// BindOwnNameForTest gives a test kernel its own name the way boot does (D15), under a fixed test
// key when no signing key is configured yet, so ResolveKernel knows "this kernel" from the start.
func (k *Kernel) BindOwnNameForTest(ctx context.Context) {
	if k.SelfKey(ctx) == "" {
		_ = k.store.SetConfig(ctx, "kernel_address", KernelAddress(testOwnKey))
	}
	if err := k.BindOwnName(ctx, TestOwnName); err != nil {
		panic(err)
	}
}

var testOwnKey = KeyFromSeed(bytes.Repeat([]byte{7}, 32))

// TestOwnKey is the key every test kernel's database is bound to.
var TestOwnKey = testOwnKey

// GenKey makes a kernel's key for a test: its address, as payloads carry it, and the key.
func GenKey() (string, *ecdsa.PrivateKey, error) {
	key, err := ethcrypto.GenerateKey()
	if err != nil {
		return "", nil, err
	}
	return KernelAddress(key), key, nil
}

// KeyFromSeed is the key whose secret is seed, for a test that needs the same kernel every run.
func KeyFromSeed(seed []byte) *ecdsa.PrivateKey {
	key, err := ethcrypto.ToECDSA(seed)
	if err != nil {
		panic(err)
	}
	return key
}

// SignRaw signs bytes the way a kernel signs a payload (P1), for a test that builds the payload
// itself.
func SignRaw(key *ecdsa.PrivateKey, payload []byte) []byte {
	digest := sha256.Sum256(payload)
	sig, err := ethcrypto.Sign(digest[:], key)
	if err != nil {
		panic(err)
	}
	return sig
}

// CanCall exposes the visibility predicate so an external test can hold the store's SQL copy of it
// to the same answers.
var CanCall = canCall

// SignTaskNoticeForTest signs a notice as the holder whose key is priv would, addressed to k (P8).
func (k *Kernel) SignTaskNoticeForTest(priv *ecdsa.PrivateKey, n TaskNotice) *SignedTaskNotice {
	ts := time.Now().UTC().Format(time.RFC3339)
	holder := KernelAddress(priv)
	sig, _ := k.cfg.Network.sign(priv, sigDomainTaskNotice, taskNoticePayload{Counterparty: holder, Notice: n, Recipient: k.ourAddress(), Timestamp: ts})
	return &SignedTaskNotice{Notice: n, Timestamp: ts, Signature: sig}
}
