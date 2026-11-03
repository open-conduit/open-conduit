//go:build linux

package claudecode

import (
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

// TestProcGone: a /proc read that fails because the process was reaped,
// before the open (ENOENT) or between the open and the read (ESRCH, an
// adopted turn exiting while it is observed), reports the process absent
// rather than failing the wait. Any other error is still an error.
func TestProcGone(t *testing.T) {
	for _, c := range []struct {
		err  error
		gone bool
	}{
		{&fs.PathError{Op: "open", Path: "/proc/1/stat", Err: syscall.ENOENT}, true},
		{&fs.PathError{Op: "read", Path: "/proc/1/stat", Err: syscall.ESRCH}, true},
		{fmt.Errorf("wrapped: %w", syscall.ESRCH), true},
		{&fs.PathError{Op: "read", Path: "/proc/1/stat", Err: syscall.EACCES}, false},
	} {
		if got := procGone(c.err); got != c.gone {
			t.Errorf("procGone(%v) = %t, want %t", c.err, got, c.gone)
		}
	}
}
