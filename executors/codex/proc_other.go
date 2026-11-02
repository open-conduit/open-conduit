//go:build !unix

package codex

import (
	"errors"
	"os/exec"
	"syscall"
)

const processSupported = false

var errUnsupportedPlatform = errors.New("codex executor requires a Unix platform")

func detach(*exec.Cmd)                           {}
func signalGroup(int, syscall.Signal) error      { return errUnsupportedPlatform }
func groupAlive(int) bool                        { return false }
func lockFile(string) (func(), error)            { return nil, errUnsupportedPlatform }
func inspectProcess(int) (procInfo, bool, error) { return procInfo{}, false, errUnsupportedPlatform }
func scanProcesses() ([]procInfo, error)         { return nil, errUnsupportedPlatform }

var (
	sigTerm = syscall.Signal(15)
	sigKill = syscall.Signal(9)
)
