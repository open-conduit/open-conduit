package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// A failed toolbelt call says what failed: the daemon's refusal, a call that
// ran out of time, a daemon that could not be reached, or a connection that
// broke mid-answer. Only the third reads "unreachable".
func TestCallErrorSaysWhatFailed(t *testing.T) {
	post := func(err error) error {
		return &url.Error{Op: "Post", URL: "http://localhost/v1/toolbelt/call", Err: err}
	}
	dial := &net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"refused", &client.Error{Status: 403, Code: "forbidden", Message: "operation not granted"}, "403 forbidden: operation not granted"},
		{"deadline", fmt.Errorf("%w: %w", context.DeadlineExceeded, post(context.DeadlineExceeded)), "no answer from the management endpoint within 1m0s"},
		{"unreachable", fmt.Errorf("%w: %w", client.ErrUnreachable, post(dial)), "management endpoint unreachable: connection refused"},
		{"broken", post(io.ErrUnexpectedEOF), "management call failed: Post \"http://localhost/v1/toolbelt/call\": unexpected EOF"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := callError(c.err)
			if !strings.HasPrefix(got, c.want) {
				t.Fatalf("callError = %q, want prefix %q", got, c.want)
			}
			if c.name != "unreachable" && strings.Contains(got, "unreachable") {
				t.Fatalf("callError = %q blames the endpoint", got)
			}
		})
	}
}
