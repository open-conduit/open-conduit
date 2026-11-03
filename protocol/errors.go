package protocol

import (
	"context"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Error is an authored protocol error: a stable code and a short,
// secret-free message. It crosses the wire as a JSON-RPC error with code
// -32000 and data.code, and maps back onto the public sentinel errors, so
// errors.Is(err, extension.ErrNotAdoptable) holds on the receiving side.
type Error struct {
	Code    string
	Message string
	// ResetAt is carried by CodeRetryAfter (extension.RetryHint).
	ResetAt *time.Time
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "protocol: " + e.Code
	}
	return "protocol: " + e.Code + ": " + e.Message
}

// sentinels maps protocol codes onto the public Go errors they carry.
var sentinels = []struct {
	code string
	err  error
}{
	{CodeInvalid, extension.ErrInvalid},
	{CodeUnsupported, extension.ErrUnsupported},
	{CodeUnauthorized, extension.ErrUnauthorized},
	{CodeUndeclared, extension.ErrUndeclared},
	{CodeNotFound, storage.ErrNotFound},
	{CodeConflict, storage.ErrConflict},
	{CodeNotAdoptable, extension.ErrNotAdoptable},
	{CodeNotAccepting, extension.ErrNotAccepting},
	{CodeNothingToAttach, extension.ErrNothingToAttach},
	{CodeNotLive, extension.ErrNotLive},
	{CodeResourceSetup, extension.ErrResourceSetup},
	{CodeResourceNotFound, extension.ErrResourceNotFound},
	{CodeResourceExhausted, extension.ErrResourceExhausted},
	{CodeUnsupportedVersion, ErrIncompatible},
	{CodeCancelled, context.Canceled},
}

// confinement are the sandbox and container refusals, whose messages cross
// with them.
var confinement = []struct {
	code string
	err  error
}{
	{CodeSandboxDenied, extension.ErrSandboxDenied},
	{CodeSandboxUnavailable, extension.ErrSandboxUnavailable},
	{CodeContainerRunning, extension.ErrContainerRunning},
	{CodeContainerUnknown, extension.ErrContainerUnconfirmed},
}

// Unwrap returns the public sentinel the code carries, if any.
func (e *Error) Unwrap() error {
	for _, s := range append(sentinels[:len(sentinels):len(sentinels)], confinement...) {
		if s.code == e.Code {
			return s.err
		}
	}
	return nil
}

// RetryAfter is a received CodeRetryAfter error: the only authored error
// that is an extension.RetryHintProvider, so a peer's other refusals (not
// adoptable, invalid, internal) never read as "wait and retry". Its
// Err carries the code, message and reset.
type RetryAfter struct{ Err *Error }

func (e RetryAfter) Error() string { return e.Err.Error() }

// RetryHint implements extension.RetryHintProvider.
func (e RetryAfter) RetryHint() extension.RetryHint {
	return extension.RetryHint{ResetAt: e.Err.ResetAt}
}

// Unwrap returns the authored error, so errors.As finds the *Error.
func (e RetryAfter) Unwrap() error { return e.Err }

// Errorf returns an authored protocol error.
func Errorf(code, message string) error { return &Error{Code: code, Message: message} }

// Encode turns any Go error into the authored error that crosses the wire.
// Public sentinels map to their codes; an extension.OperationError crosses
// verbatim; a RetryHintProvider becomes CodeRetryAfter. Anything else is
// unauthored and crosses as CodeInternal with no detail, which stays in the
// sender's own log.
func Encode(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	var oe *extension.OperationError
	if errors.As(err, &oe) {
		return &Error{Code: oe.Code, Message: oe.Message}
	}
	var hint extension.RetryHintProvider
	if errors.As(err, &hint) {
		return &Error{Code: CodeRetryAfter, Message: "retryable executor limit", ResetAt: hint.RetryHint().ResetAt}
	}
	if errors.Is(err, storage.ErrLeaseLost) || errors.Is(err, storage.ErrClaimed) {
		return &Error{Code: CodeConflict, Message: "lease fenced"}
	}
	if errors.Is(err, storage.ErrInvalid) {
		return &Error{Code: CodeInvalid, Message: "invalid"}
	}
	if errors.Is(err, extension.ErrConflict) {
		return &Error{Code: CodeConflict, Message: "registration conflict"}
	}
	if errors.Is(err, extension.ErrResourceExhausted) {
		// The refusal says what to free; providers author it from sizes and
		// workspace names only.
		return &Error{Code: CodeResourceExhausted, Message: err.Error()}
	}
	// A sandbox or container refusal says which need or launch it refused,
	// from paths and the provider's own words, so the executor can report it.
	for _, s := range confinement {
		if errors.Is(err, s.err) {
			return &Error{Code: s.code, Message: err.Error()}
		}
	}
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			return &Error{Code: s.code, Message: s.code}
		}
	}
	return &Error{Code: CodeInternal, Message: "internal error"}
}

// Decode returns the Go error for an authored error received from a peer:
// a sentinel-carrying *Error for protocol codes, and an
// extension.OperationError for an extension's own operation codes.
func Decode(code, message string, resetAt *time.Time) error {
	if code == CodeRetryAfter {
		return RetryAfter{&Error{Code: code, Message: message, ResetAt: resetAt}}
	}
	if Known(code) {
		return &Error{Code: code, Message: message}
	}
	return &extension.OperationError{Code: code, Message: message}
}

// Known reports whether code is one of the protocol's own codes.
func Known(code string) bool {
	switch code {
	case CodeUnsupportedVersion, CodeMethodNotFound, CodeInvalid, CodeUnsupported, CodeUnauthorized, CodeUndeclared, CodeNotFound, CodeConflict, CodeCausalLimit, CodeNotAdoptable, CodeNotAccepting, CodeNothingToAttach, CodeNotLive, CodeResourceSetup, CodeResourceNotFound, CodeResourceExhausted, CodeSandboxDenied, CodeSandboxUnavailable, CodeContainerRunning, CodeContainerUnknown, CodeRetryAfter, CodeReentrant, CodeOverloaded, CodeCancelled, CodeShuttingDown, CodeInternal, CodeSchema, CodeKVDowngrade:
		return true
	}
	return false
}

// ErrorCode returns err's protocol code, or "" when err is not authored.
func ErrorCode(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
