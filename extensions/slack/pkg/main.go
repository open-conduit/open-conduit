package slackext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run slack` does (#428). Its subcommands
// package-manifest and package-certification print the package files, and
// certify-fixture serves the conformance runner's fixture. It returns the
// process's exit status.
func Main(args []string) int {
	b, err := New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, Certification(), os.Stdout) {
		return 0
	}
	if len(args) == 1 && args[0] == "certify-fixture" {
		if err := ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	_ = b.Serve()
	return 0
}
