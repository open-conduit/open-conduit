//go:build unix

package codex

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const processSupported = true

// detach starts the wrapper as a session and process-group leader so the whole
// tree can be signaled together and it survives the daemon.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalGroup signals every member of a process group.
func signalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return errors.New("invalid process group")
	}
	err := syscall.Kill(-pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// groupAlive reports whether any process remains in the group. A process ID
// cannot be reused while a process group with that ID still has members.
func groupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockFile takes an exclusive advisory lock that serializes finalization across
// executor instances, including a restarted daemon.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

var (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)
