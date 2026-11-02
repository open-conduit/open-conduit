package sdk

import (
	"os"
	"sync"
)

var (
	selfMu   sync.Mutex
	selfArgv []string
)

// SetSelf records the command that runs this extension again, for a
// process that serves it from a multicall binary: the host's own
// `open-conduit extension-run <name>` sets it, so an extension that
// re-executes itself (an executor's detached segments, a certification
// fake) runs the same extension rather than the host.
func SetSelf(argv []string) {
	selfMu.Lock()
	defer selfMu.Unlock()
	selfArgv = append([]string(nil), argv...)
}

// Self is the command that runs this extension again, to which a
// subcommand's arguments are appended: what SetSelf recorded, else this
// process's own executable.
func Self() ([]string, error) {
	selfMu.Lock()
	argv := append([]string(nil), selfArgv...)
	selfMu.Unlock()
	if len(argv) > 0 {
		return argv, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe}, nil
}
