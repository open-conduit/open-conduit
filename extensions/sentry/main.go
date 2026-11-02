package sentry

import (
	"fmt"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"os"
)

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run sentry` does (#428). Its subcommands:
// package-manifest and package-certification print the package files, and certify-fixture serves the conformance runner's fixture.
// It returns the process's exit status.
func Main(args []string) int {
	if sdk.PackageCommand(args, Manifest(), Certification(), os.Stdout) {
		return 0
	}
	if len(args) == 1 && args[0] == "certify-fixture" {
		if err := ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	_ = sdk.Serve(Build(Options{}))
	return 0
}
