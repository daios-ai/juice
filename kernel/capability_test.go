// SPDX-License-Identifier: AGPL-3.0-only

package kernel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestCapabilityIssueVerify(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "owner", 1000)
	a := setupAction(t, st, owner.ID, "svc", 0)
	_, tr := beginTestRun(t, st, owner.ID, a) // live, unsettled trace

	tok, err := k.IssueCapability(tr.ID)
	if err != nil {
		t.Fatalf("IssueCapability: %v", err)
	}
	gotTrace, gotOwner, err := k.VerifyCapability(ctx, tok)
	if err != nil {
		t.Fatalf("VerifyCapability: %v", err)
	}
	if gotTrace != tr.ID {
		t.Errorf("trace: got %s, want %s", gotTrace, tr.ID)
	}
	if gotOwner != owner.ID {
		t.Errorf("owner: got %s, want %s (trace.action_owner_id)", gotOwner, owner.ID)
	}

	// TraceHasTransaction is false for an unsettled trace.
	if settled, _ := st.TraceHasTransaction(ctx, tr.ID); settled {
		t.Error("unsettled trace reported as having a transaction")
	}

	// Tampered signature and unknown trace are rejected.
	if _, _, err := k.VerifyCapability(ctx, tok+"x"); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("tampered token: want ErrUnauthorized, got %v", err)
	}
	if _, _, err := k.VerifyCapability(ctx, "nope"); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("malformed token: want ErrUnauthorized, got %v", err)
	}
	bogus, _ := k.IssueCapability("00000000-0000-0000-0000-0000000000ff")
	if _, _, err := k.VerifyCapability(ctx, bogus); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("unknown-trace token: want ErrUnauthorized, got %v", err)
	}
}

func TestCapabilityRejectedAfterSettlement(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	sys := setupUser(t, st, "sys", 0)
	k.RegisterNativeHandler("echo", func(_ context.Context, _ map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return map[string]any{}, nil
	})
	a, err := k.RegisterNativeAction(ctx, kernel.CreateActionRequest{OwnerUserID: sys.ID, Name: "echo", Kind: kernel.KindNative})
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{"type": "object"}
	if err := k.ActivateNativeAction(ctx, a.ID, "echo native", obj, obj, 0, ""); err != nil {
		t.Fatal(err)
	}

	reply, err := k.Run(ctx, kernel.RunRequest{CallerID: sys.ID, ActionRef: "sys@k/echo", Args: map[string]any{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The root call's trace is now settled (a transaction exists for it).
	if settled, _ := st.TraceHasTransaction(ctx, reply.TraceID); !settled {
		t.Fatal("settled trace should have a transaction")
	}
	tok, _ := k.IssueCapability(reply.TraceID)
	if _, _, err := k.VerifyCapability(ctx, tok); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("capability after settlement: want ErrUnauthorized, got %v", err)
	}
}
