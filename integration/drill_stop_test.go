package integration_test

import (
	"syscall"
	"time"
)

// stop is a graceful SIGTERM stop (a drained switch, not a crash).
func (d *drill) stop() {
	d.t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		d.t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(30 * time.Second):
		d.t.Fatal("daemon ignored SIGTERM")
	}
	if !d.cmd.ProcessState.Success() {
		d.t.Fatalf("graceful exit: %v", d.cmd.ProcessState)
	}
}
