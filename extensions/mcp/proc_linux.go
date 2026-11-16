//go:build linux

package mcp

import (
	"os/exec"
	"runtime"
	"syscall"
)

// A server stays in the package's process group, so the host's kill of
// the package's group reaches it, and on Linux it is also killed when the
// package dies however it dies (Pdeathsig). The kernel sends that signal
// when the thread that forked the server exits, so every server is
// started from one thread that is locked for the process's life.
var starts = make(chan startReq)

type startReq struct {
	cmd  *exec.Cmd
	done chan error
}

func init() {
	go func() {
		runtime.LockOSThread()
		for r := range starts {
			r.done <- r.cmd.Start()
		}
	}()
}

func startServer(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	r := startReq{cmd: cmd, done: make(chan error, 1)}
	starts <- r
	return <-r.done
}
