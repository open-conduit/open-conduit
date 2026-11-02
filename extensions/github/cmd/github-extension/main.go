// Command github-extension serves the first-party GitHub extension as a
// subprocess package over protocol version 1, through protocol/sdk/inprocsdk
// (#357). Subcommands: package-manifest and package-certification print the
// package files, and certify-fixture serves the conformance runner's GitHub
// fake.
package main

import (
	"fmt"
	"os"

	githubext "github.com/open-conduit/open-conduit/extensions/github/pkg"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

func main() {
	args := os.Args[1:]
	b, err := githubext.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, githubext.Certification(), os.Stdout) {
		return
	}
	if len(args) == 1 && args[0] == "certify-fixture" {
		if err := githubext.ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	_ = b.Serve()
}
