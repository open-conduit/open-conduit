// Command shell-extension serves the first-party shell executor as a
// subprocess package over protocol version 1 (07-03). Subcommands
// package-manifest and package-certification print the package files.
package main

import (
	"os"
	"sync/atomic"

	shellext "github.com/open-conduit/open-conduit/packages/shell"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

func main() {
	if sdk.PackageCommand(os.Args[1:], shellext.Manifest(), shellext.Certification(), os.Stdout) {
		return
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	var server atomic.Pointer[sdk.Server]
	ext := shellext.Build(func() string {
		if s := server.Load(); s != nil {
			return s.StateDir()
		}
		return ""
	})
	s := sdk.NewServer(ext, os.Stdin, out)
	server.Store(s)
	<-s.Done()
}
