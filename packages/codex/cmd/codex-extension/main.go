// Command codex-extension serves the Codex CLI executor as a subprocess
// package over protocol version 1. Subcommands: package-manifest and
// package-certification print the package files, segment drives one
// detached segment (the executor starts it), certify-fixture serves the
// conformance runner's fixture and fake-codex is the fake CLI it names.
package main

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/open-conduit/open-conduit/executors/codex"
	codexext "github.com/open-conduit/open-conduit/packages/codex"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

func main() {
	args := os.Args[1:]
	if sdk.PackageCommand(args, codexext.Manifest(), codexext.Certification(), os.Stdout) {
		return
	}
	if len(args) > 0 {
		switch args[0] {
		case codex.SegmentCommand:
			os.Exit(codex.RunSegment(args[1:]))
		case "fake-codex":
			os.Exit(codexext.FakeCodex(args[1:], os.Stdin, os.Stdout))
		case "certify-fixture":
			if err := codexext.ServeFixture(os.Stdin, os.Stdout); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	var server atomic.Pointer[sdk.Server]
	ext := codexext.Build(func() string {
		if s := server.Load(); s != nil {
			return s.StateDir()
		}
		return ""
	})
	s := sdk.NewServer(ext, os.Stdin, out)
	server.Store(s)
	<-s.Done()
}
