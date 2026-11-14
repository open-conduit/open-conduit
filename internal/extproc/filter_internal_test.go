package extproc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// TestFilterRefusedRetriesNoAnswer: a dependency.filter call that got no
// answer (a timeout, a dead transport, an overloaded or restarting
// process) is asked again, while the extension's own refusal is a definite
// "no match" (#613).
func TestFilterRefusedRetriesNoAnswer(t *testing.T) {
	for _, err := range []error{
		protocol.ErrTransport,
		context.DeadlineExceeded,
		protocol.Errorf(protocol.CodeOverloaded, "busy"),
		protocol.Errorf(protocol.CodeShuttingDown, ""),
		protocol.Errorf(protocol.CodeCancelled, ""),
		protocol.Errorf(protocol.CodeInternal, ""),
		errors.New("broken pipe"),
	} {
		if filterRefused(err) {
			t.Errorf("%v counted as the extension's answer; it must be asked again", err)
		}
	}
	for _, code := range []string{protocol.CodeSchema, protocol.CodeInvalid, protocol.CodeUndeclared, protocol.CodeMethodNotFound} {
		if err := fmt.Errorf("call: %w", protocol.Errorf(code, "")); !filterRefused(err) {
			t.Errorf("%s was retried; it is the extension's definite refusal", code)
		}
	}
}

// TestFilterAnsweredSeparatesFailureFromOutage: only a failure the live
// extension returned (an internal error from its handler) counts as its
// answer; no answer at all (a dead transport, the deadline, an overloaded
// or draining process) is an outage, which core holds through however
// long it lasts.
func TestFilterAnsweredSeparatesFailureFromOutage(t *testing.T) {
	for _, err := range []error{
		protocol.ErrTransport,
		context.DeadlineExceeded,
		context.Canceled,
		protocol.Errorf(protocol.CodeOverloaded, ""),
		protocol.Errorf(protocol.CodeShuttingDown, ""),
		protocol.Errorf(protocol.CodeCancelled, ""),
		protocol.Errorf(protocol.CodeReentrant, ""),
		errors.New("broken pipe"),
	} {
		if filterAnswered(err) {
			t.Errorf("%v counted as the extension's own failure; it is an outage", err)
		}
	}
	if !filterAnswered(protocol.Errorf(protocol.CodeInternal, "")) {
		t.Error("an internal error from the live extension counted as an outage")
	}
}
