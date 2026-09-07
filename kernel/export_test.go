package kernel

import "context"

// Test-only seam (the standard export_test.go idiom): the call engine and its request are private,
// because most orchestration modes — a pre-funded trace, a step id, an inbound idempotency record —
// are only valid when the kernel itself supplies them (§4). The package's external tests still need
// to drive those modes directly, so they reach them here rather than through an exported surface a
// client could misuse.
type TestCallRequest = callRequest

// TestCall drives the private engine with any orchestration mode.
func (k *Kernel) TestCall(ctx context.Context, req TestCallRequest) (*CallReply, error) {
	return k.call(ctx, req)
}

// DispatchRecordForTest builds the record beginRun freezes on a dispatched trace: the rates, the
// secret and the lottery this call is committed to. A test that stages a proxy trace by hand needs
// it, because settlement reads every pricing input from here and nowhere else (§13).
func DispatchRecordForTest(mp, gross, remoteBPS, importBPS, lottery int64, secret string) *string {
	return marshalDispatch(nil, "", mp, gross, "", remoteBPS, importBPS, secret, lottery)
}

// ServingRecordForTest is the seller's half of the same record: what a foreign call was admitted
// under, for a test that stages one by hand.
func ServingRecordForTest(remoteBPS, lottery, reserve int64, nonce, commitment string) *string {
	return marshalServing(remoteBPS, lottery, reserve, nonce, commitment)
}
