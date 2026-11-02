package shellext

import (
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"os"
	"sync/atomic"
)

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run shell` does (#428). Its subcommands:
// package-manifest and package-certification print the package files.
// It returns the process's exit status.
func Main(args []string) int {
	if sdk.PackageCommand(args, Manifest(), Certification(), os.Stdout) {
		return 0
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	var server atomic.Pointer[sdk.Server]
	ext := Build(func() string {
		if s := server.Load(); s != nil {
			return s.StateDir()
		}
		return ""
	})
	s := sdk.NewServer(ext, os.Stdin, out)
	server.Store(s)
	<-s.Done()
	return 0
}
