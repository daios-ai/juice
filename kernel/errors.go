package kernel

import "fmt"

// KernelError is a typed error with a stable code and default HTTP status.
type KernelError struct {
	Code    string
	HTTP    int
	Message string
}

func (e *KernelError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// Wrap returns a new KernelError with an attached message.
func (e *KernelError) Wrap(msg string) *KernelError {
	return &KernelError{Code: e.Code, HTTP: e.HTTP, Message: msg}
}

// Wrapf returns a new KernelError with a formatted message.
func (e *KernelError) Wrapf(format string, args ...any) *KernelError {
	return &KernelError{Code: e.Code, HTTP: e.HTTP, Message: fmt.Sprintf(format, args...)}
}

// Is implements errors.Is so callers can match on the sentinel values.
func (e *KernelError) Is(target error) bool {
	t, ok := target.(*KernelError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// HTTPStatus returns the HTTP status code for this error, or 500 if unset.
func HTTPStatus(err error) int {
	if ke, ok := err.(*KernelError); ok && ke.HTTP != 0 {
		return ke.HTTP
	}
	return 500
}

// kernelErrorCode returns the stable error code string, or "internal" if unknown.
func kernelErrorCode(err error) string {
	if ke, ok := err.(*KernelError); ok {
		return ke.Code
	}
	return "internal"
}

// HTTPStatusFromCode maps a stored error code back to an HTTP status.
func HTTPStatusFromCode(code string) int {
	for _, sentinel := range []*KernelError{
		ErrUnauthenticated, ErrUnauthorized, ErrNotFound, ErrInvalidInput,
		ErrInvalidState, ErrInsufficientFunds, ErrExecutionFailed,
		ErrSchemaViolation, ErrTimeout, ErrInternal,
	} {
		if sentinel.Code == code {
			return sentinel.HTTP
		}
	}
	return 500
}

// Sentinel errors.
var (
	ErrUnauthenticated   = &KernelError{Code: "unauthenticated", HTTP: 401}
	ErrUnauthorized      = &KernelError{Code: "unauthorized", HTTP: 403}
	ErrNotFound          = &KernelError{Code: "not_found", HTTP: 404}
	ErrInvalidInput      = &KernelError{Code: "invalid_input", HTTP: 422}
	ErrInvalidState      = &KernelError{Code: "invalid_state", HTTP: 409}
	ErrInsufficientFunds = &KernelError{Code: "insufficient_funds", HTTP: 402}
	ErrExecutionFailed   = &KernelError{Code: "execution_failed", HTTP: 500}
	ErrSchemaViolation   = &KernelError{Code: "schema_violation", HTTP: 422}
	ErrTimeout           = &KernelError{Code: "timeout", HTTP: 504}
	ErrInternal          = &KernelError{Code: "internal", HTTP: 500}
)
