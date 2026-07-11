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
		ErrPeerUnreachable, ErrPeerUnfunded,
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
	// ErrPeerUnreachable: the first dispatch of a remote-proxy call provably never reached the
	// peer (§13 never-dispatched); the call is settled locally with a full refund. 502 (bad
	// gateway), distinct from ErrTimeout's 504 (parked, awaiting a receipt). Meta["peer"] names it.
	ErrPeerUnreachable = &KernelError{Code: "peer_unreachable", HTTP: 502}
	// ErrPeerUnfunded: this kernel's prepaid credit on the peer is exhausted (§13); the peer
	// signed a zero-charge rejection. An operator condition (out-of-band payment + admin deposit),
	// never the caller's own balance — hence a distinct code carrying Meta["peer"], HTTP 402.
	ErrPeerUnfunded = &KernelError{Code: "peer_unfunded", HTTP: 402}
)

// GrantRequiredError is the one lazy-consent rejection (§8): ref in both the message and
// Meta["action"], so every mint site is identical and clients always get a qualified @owner/name.
func GrantRequiredError(ref string) error {
	return ErrGrantRequired.Wrapf("grant required for %s", ref).WithMeta("action", ref)
}

// PeerUnreachableError attributes a never-dispatched remote call to the peer (§13): handle in
// both the message and Meta["peer"], mirroring GrantRequiredError.
func PeerUnreachableError(handle string) *KernelError {
	return ErrPeerUnreachable.Wrapf("peer %s is unreachable; the call was not sent and has been refunded", handle).WithMeta("peer", handle)
}

// PeerUnfundedError attributes a 402 signed rejection to this kernel's exhausted credit on the
// peer (§13): handle in the message and Meta["peer"]. An operator condition, not the caller's.
func PeerUnfundedError(handle string) *KernelError {
	return ErrPeerUnfunded.Wrapf("this kernel's credit with peer %s is exhausted; the operator must top up", handle).WithMeta("peer", handle)
}
