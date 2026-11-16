//go:build !linux

package mcp

import "os/exec"

// A server stays in the package's process group, so the host's kill of
// the package's group reaches it.
func startServer(cmd *exec.Cmd) error { return cmd.Start() }
