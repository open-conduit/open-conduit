//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// setGroup starts the server in a process group of its own, so stopping it
// stops what it started.
func setGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
