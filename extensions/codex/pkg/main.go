package codexext

import (
	"fmt"
	"github.com/open-conduit/open-conduit/extensions/codex"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"os"
	"sync/atomic"
)

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run codex` does (#428). Its subcommands:
// package-manifest and package-certification print the package files, segment drives one detached segment (the executor starts it through sdk.Self), certify-fixture serves the conformance runner's fixture and fake-codex is the fake CLI it names.
// It returns the process's exit status.
func Main(args []string) int {
	if sdk.PackageCommand(args, Manifest(), Certification(), os.Stdout) {
		return 0
	}
	if len(args) > 0 {
		switch args[0] {
		case codex.SegmentCommand:
			return codex.RunSegment(args[1:])
		case "fake-codex":
			return FakeCodex(args[1:], os.Stdin, os.Stdout)
		case "certify-fixture":
			if err := ServeFixture(os.Stdin, os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			return 0
		}
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
