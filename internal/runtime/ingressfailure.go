package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
)

// Provider failure classes: what kind of failure a reconcile page met, so
// an operator can tell a slow provider from a broken package or an upstream
// refusal without reading the provider's code.
const (
	// IngressFailureTimeout: the provider gave no page within PageTimeout.
	IngressFailureTimeout = "timeout"
	// IngressFailureCancelled: the call was cancelled, by the daemon
	// stopping or by the provider's side.
	IngressFailureCancelled = "cancelled"
	// IngressFailureTransport: the package's connection closed mid-call.
	IngressFailureTransport = "transport"
	// IngressFailureProtocol: the provider refused with one of the
	// protocol's own codes (invalid, unauthorized, overloaded, ...).
	IngressFailureProtocol = "protocol"
	// IngressFailureUnauthored: the provider failed without authoring an
	// error. Its words stay in the extension's own log.
	IngressFailureUnauthored = "unauthored"
	// IngressFailureProvider: the provider authored its own code (an
	// extension.OperationError), such as an upstream API refusal.
	IngressFailureProvider = "provider"
)

// ingressFailureMessageLimit bounds a recorded failure message.
const ingressFailureMessageLimit = 256

// IngressFailure is what a provider reported when its reconcile page
// failed: its class, its code and a bounded message. Code and Message come
// only from authored errors (protocol.Encode), which are secret-free by
// contract; an unauthored error's own text is never recorded.
type IngressFailure struct {
	Class   string `json:"class"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// String is the failure in one line: class, code and message.
func (f IngressFailure) String() string {
	out := f.Class
	if f.Code != "" && f.Code != f.Class {
		out += "/" + f.Code
	}
	if f.Message != "" {
		out += ": " + f.Message
	}
	return out
}

// classifyProviderFailure names the failure of one reconcile call. parent is
// the scan's own context and call the page's, bounded by timeout.
func classifyProviderFailure(parent, call context.Context, err error, timeout time.Duration) *IngressFailure {
	switch {
	case parent.Err() == nil && errors.Is(call.Err(), context.DeadlineExceeded):
		return &IngressFailure{Class: IngressFailureTimeout, Code: IngressFailureTimeout, Message: "no page within " + timeout.String()}
	case parent.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return &IngressFailure{Class: IngressFailureCancelled, Code: IngressFailureCancelled}
	case errors.Is(err, protocol.ErrTransport):
		return &IngressFailure{Class: IngressFailureTransport, Code: "transport_closed", Message: "the package's connection closed during the call"}
	}
	e := protocol.Encode(err)
	f := &IngressFailure{Class: IngressFailureProvider, Code: boundFailureText(e.Code, 64), Message: boundFailureText(e.Message, ingressFailureMessageLimit)}
	switch {
	case e.Code == protocol.CodeInternal:
		f.Class, f.Message = IngressFailureUnauthored, "unauthored error; the extension's log has its detail"
	case e.Code == protocol.CodeCancelled:
		f.Class = IngressFailureCancelled
	case protocol.Known(e.Code):
		f.Class = IngressFailureProtocol
	}
	if e.ResetAt != nil {
		f.Message = strings.TrimSpace(f.Message + " (until " + e.ResetAt.UTC().Format(time.RFC3339) + ")")
	}
	return f
}

// boundFailureText keeps text to one line of at most n bytes.
func boundFailureText(s string, n int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' }), " ")
	return truncate(s, n)
}
