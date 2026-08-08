package kernel

import (
	"errors"
	"fmt"
	"strconv"
)

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

// ErrorFromCode maps a stored error code back to its sentinel, so a code that crossed a
// process or kernel boundary can be re-raised as the same typed error. Unknown codes — an
// older or newer peer — degrade to ErrExecutionFailed rather than being silently dropped.
func ErrorFromCode(code string) *KernelError {
	for _, sentinel := range []*KernelError{
		ErrUnauthenticated, ErrUnauthorized, ErrNotFound, ErrInvalidInput,
		ErrInvalidState, ErrInsufficientFunds, ErrExecutionFailed,
		ErrSchemaViolation, ErrTimeout, ErrInternal, ErrGrantRequired,
		ErrPeerUnreachable, ErrPeerUnfunded, ErrTermsChanged,
	} {
		if sentinel.Code == code {
			return sentinel
		}
	}
	return ErrExecutionFailed
}

// HTTPStatusFromCode maps a stored error code back to an HTTP status. An unknown or empty code
// yields ErrExecutionFailed's 500, matching the pre-lookup behavior.
func HTTPStatusFromCode(code string) int {
	return ErrorFromCode(code).HTTP
}

// ErrStepNotClaimed marks a completion that never took the step: another completer already holds
// it, or it was already resolved. It is a *cause* attached with Because(), not a new error code —
// the outer error stays ErrInvalidState, so the wire status, KernelErrorCode, and the ErrorFromCode
// table are all unchanged, while errors.Is can still tell "someone else got there first" apart from
// "the resumed call failed". A distinct KernelError could not do this: KernelError.Is compares Code
// alone, so two sentinels sharing invalid_state are indistinguishable.
//
// Note the construction order: Wrap() drops the cause, so it must be Wrap(...).Because(...).
var ErrStepNotClaimed = errors.New("step not claimed by this completion")

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
	// ErrTermsChanged: a run's quote pin no longer matches (§4 precondition 7). Distinct from
	// ErrInvalidState, which it shares a status with, because the action is perfectly callable —
	// only at a price the caller has not agreed to — and a client must tell "re-confirm the new
	// terms" from "this action is disabled" by code, never by sniffing Meta.
	ErrTermsChanged = &KernelError{Code: "terms_changed", HTTP: 409}
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

// TermsChangedError refuses a run whose quote pin no longer matches (§4 precondition 7), before any
// funds are locked. Meta carries the current hash AND price: the hash alone would let a client
// blindly re-arm and retry, defeating the pin, while the price is what a human re-consents to.
func TermsChangedError(currentHash string, currentPrice int64) error {
	return ErrTermsChanged.Wrapf("the action's terms changed; it now costs %d", currentPrice).
		WithMeta("quote_hash", currentHash).WithMeta("price", strconv.FormatInt(currentPrice, 10))
}

// PeerUnfundedError attributes a 402 signed rejection to this kernel's exhausted credit on the
// peer (§13): handle in the message and Meta["peer"]. An operator condition, not the caller's.
func PeerUnfundedError(handle string) *KernelError {
	return ErrPeerUnfunded.Wrapf("this kernel's credit with peer %s is exhausted; the operator must top up", handle).WithMeta("peer", handle)
}
