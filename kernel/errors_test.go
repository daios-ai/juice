package kernel

import (
	"errors"
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

func TestHTTPStatus(t *testing.T) {
	cases := []struct {
		err    error
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
