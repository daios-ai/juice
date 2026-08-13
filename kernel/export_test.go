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
