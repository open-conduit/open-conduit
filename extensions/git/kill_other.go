//go:build windows

package git

import (
	"context"
	"os/exec"
)

// Windows has no supported local transport yet (B06); setup runs without
// process-group cleanup and removal does not search for stray processes.
func runGroup(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	}
}

func killWithin(string) {}

// inUse can't tell here, so a sweep keeps the workspace.
func inUse(string) bool { return true }
