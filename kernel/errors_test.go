// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"errors"
	"strings"
	"testing"
)

func TestKernelErrorError(t *testing.T) {
	e := &KernelError{Code: "not_found", HTTP: 404, Message: "thing not found"}
	if e.Error() != "thing not found" {
		t.Errorf("Error(): got %q, want %q", e.Error(), "thing not found")
	}
	e2 := &KernelError{Code: "not_found", HTTP: 404}
	if e2.Error() != "not_found" {
		t.Errorf("Error() with no message: got %q, want %q", e2.Error(), "not_found")
	}
}

func TestKernelErrorWrap(t *testing.T) {
	wrapped := ErrNotFound.Wrap("action missing")
	if wrapped.Code != "not_found" {
		t.Errorf("Wrap preserves Code: got %q", wrapped.Code)
	}
	if wrapped.Message != "action missing" {
		t.Errorf("Wrap sets Message: got %q", wrapped.Message)
	}
	if wrapped.HTTP != 404 {
		t.Errorf("Wrap preserves HTTP: got %d", wrapped.HTTP)
	}
}

func TestKernelErrorWrapf(t *testing.T) {
	wrapped := ErrInvalidInput.Wrapf("bad value: %d", 42)
	if wrapped.Message != "bad value: 42" {
		t.Errorf("Wrapf message: got %q", wrapped.Message)
	}
	if wrapped.Code != "invalid_input" {
		t.Errorf("Wrapf preserves Code: got %q", wrapped.Code)
	}
}

func TestKernelErrorIs(t *testing.T) {
	wrapped := ErrNotFound.Wrap("missing")
	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("errors.Is should match wrapped error to sentinel")
	}
	if errors.Is(wrapped, ErrUnauthorized) {
		t.Error("errors.Is should not match different sentinel")
	}
}

func TestKernelErrorBecauseUnwrap(t *testing.T) {
	cause := errors.New("dial tcp: connection refused")
	e := ErrInvalidState.Wrap("cannot reach server").Because(cause)
	if e.Error() != "cannot reach server" {
		t.Errorf("Error() should show only the friendly message, got %q", e.Error())
	}
	if !errors.Is(e, ErrInvalidState) {
		t.Error("Because must preserve the code for errors.Is")
	}
	if errors.Unwrap(e) != cause {
		t.Error("Unwrap should return the attached cause")
	}
}

func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
	}{
		{ErrUnauthenticated, 401},
		{ErrUnauthorized, 403},
		{ErrNotFound, 404},
		{ErrInvalidInput, 422},
		{ErrInvalidState, 409},
		{ErrInsufficientFunds, 402},
		{ErrExecutionFailed, 500},
		{ErrSchemaViolation, 422},
		{ErrTimeout, 504},
		{ErrInternal, 500},
	}
	for _, tc := range cases {
		if got := HTTPStatus(tc.err); got != tc.wantStatus {
			t.Errorf("HTTPStatus(%v): got %d, want %d", tc.err, got, tc.wantStatus)
		}
	}
	if HTTPStatus(ErrNotFound.Wrap("x")) != 404 {
		t.Error("HTTPStatus should work on wrapped errors")
	}
}

func TestGrantRequiredCodeAndMeta(t *testing.T) {
	if HTTPStatusFromCode("grant_required") != 403 {
		t.Errorf("grant_required status = %d, want 403", HTTPStatusFromCode("grant_required"))
	}
	// The single constructor puts the address in the message, for a person, and the principal in
	// Meta["action"], for a program (D20), and preserves the sentinel identity.
	e := GrantRequiredError(AddressView{Address: "bob@k/a", Principal: "u1@KEY/a"}).(*KernelError)
	if e.Meta["action"] != "u1@KEY/a" {
		t.Errorf("Meta[action] = %q, want the principal u1@KEY/a", e.Meta["action"])
	}
	if !strings.Contains(e.Message, "bob@k/a") || strings.Contains(e.Message, "u1@KEY") {
		t.Errorf("message = %q, want the address bob@k/a and not the principal", e.Message)
	}
	if !errorsIs(e, ErrGrantRequired) {
		t.Error("GrantRequiredError lost the error identity")
	}
	// WithMeta copies: the sentinel is not mutated.
	if ErrGrantRequired.Meta != nil {
		t.Error("WithMeta mutated the sentinel")
	}
}

func TestPeerErrorsCodesAndMeta(t *testing.T) {
	if HTTPStatusFromCode("peer_unreachable") != 502 {
		t.Errorf("peer_unreachable status = %d, want 502", HTTPStatusFromCode("peer_unreachable"))
	}
	if HTTPStatusFromCode("peer_unfunded") != 402 {
		t.Errorf("peer_unfunded status = %d, want 402", HTTPStatusFromCode("peer_unfunded"))
	}
	peer := AddressView{Address: "beta", Principal: "KEY"}
	for _, e := range []*KernelError{PeerUnreachableError(peer), PeerUnfundedError(peer), PeerRefusedError(peer)} {
		if e.Meta["peer"] != "KEY" || !strings.Contains(e.Message, "beta") {
			t.Errorf("Meta[peer] = %q, message %q: want the key in meta and the name in the message", e.Meta["peer"], e.Message)
		}
	}
	if !errorsIs(PeerUnreachableError(peer), ErrPeerUnreachable) {
		t.Error("PeerUnreachableError lost the error identity")
	}
	if !errorsIs(PeerUnfundedError(peer), ErrPeerUnfunded) {
		t.Error("PeerUnfundedError lost the error identity")
	}
}

func errorsIs(err, target error) bool {
	ke, ok := err.(*KernelError)
	tk, ok2 := target.(*KernelError)
	return ok && ok2 && ke.Code == tk.Code
}
