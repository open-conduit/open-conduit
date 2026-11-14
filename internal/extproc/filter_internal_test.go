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
