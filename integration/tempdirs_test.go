package integration_test

import (
	"os"
	"sync"
)

// The directories helpers keep for the whole test binary (binaries built
// once, the history benchmarks' stores) live under one root, which TestMain
// removes after the run, so a run leaves nothing in the temp directory
// (#445).
var (
	runDirMu sync.Mutex
	runDir   string
)

// keptDir is a new directory, named from pattern as os.MkdirTemp names it,
// that outlives any one test; TestMain removes it after the run.
func keptDir(pattern string) (string, error) {
	runDirMu.Lock()
	defer runDirMu.Unlock()
	if runDir == "" {
		dir, err := os.MkdirTemp("", "oc-test-run-")
		if err != nil {
			return "", err
		}
		runDir = dir
	}
	return os.MkdirTemp(runDir, pattern)
}

// removeKeptDirs removes every directory keptDir made.
func removeKeptDirs() {
	runDirMu.Lock()
	defer runDirMu.Unlock()
	if runDir != "" {
		_ = os.RemoveAll(runDir)
		runDir = ""
	}
}
