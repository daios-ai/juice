package kernel

import "fmt"

// KernelError is a typed error with a stable code and default HTTP status.
// Meta carries structured, machine-actionable context (e.g. the action a grant is required
// for) so clients act on fields, not on Message substrings.
type KernelError struct {
	Code    string
	HTTP    int
	Message string
	Meta    map[string]string
	cause   error // optional underlying error; hidden from Error(), exposed via Unwrap()
}

func (e *KernelError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// WithMeta returns a copy carrying an additional structured key/value. Preserves code,
// status, message, cause, and any existing Meta.
func (e *KernelError) WithMeta(key, value string) *KernelError {
	meta := map[string]string{}
	for k, v := range e.Meta {
		meta[k] = v
	}
	meta[key] = value
	return &KernelError{Code: e.Code, HTTP: e.HTTP, Message: e.Message, Meta: meta, cause: e.cause}
}

// Because attaches an underlying cause (e.g. a raw transport error) while keeping this
// error's code, HTTP status, and message. The cause stays out of Error() but is reachable
// via Unwrap(), so a verbose renderer can surface it without polluting the default message.
func (e *KernelError) Because(cause error) *KernelError {
	return &KernelError{Code: e.Code, HTTP: e.HTTP, Message: e.Message, Meta: e.Meta, cause: cause}
}

// Unwrap returns the attached cause, if any.
func (e *KernelError) Unwrap() error { return e.cause }

// Wrap returns a new KernelError with an attached message.
func (e *KernelError) Wrap(msg string) *KernelError {
	return &KernelError{Code: e.Code, HTTP: e.HTTP, Message: msg, Meta: e.Meta}
}

// Wrapf returns a new KernelError with a formatted message.
func (e *KernelError) Wrapf(format string, args ...any) *KernelError {
	return &KernelError{Code: e.Code, HTTP: e.HTTP, Message: fmt.Sprintf(format, args...), Meta: e.Meta}
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

// KernelErrorCode returns the stable error code string, or "internal" if unknown.
func KernelErrorCode(err error) string {
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
		ErrSchemaViolation, ErrTimeout, ErrInternal, ErrGrantRequired,
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
	// ErrGrantRequired: the process owner must delegate an upstream OAuth grant before this
	// action can run (§8). A precondition with a one-time remedy, like ErrInsufficientFunds;
	// Meta["action"] names the action so any client can drive consent by field, not substring.
	ErrGrantRequired = &KernelError{Code: "grant_required", HTTP: 403}
)
