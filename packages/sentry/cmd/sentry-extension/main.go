// Command sentry-extension serves the Sentry source package over protocol
// version 1. Subcommands: package-manifest and package-certification print
// the package files, and certify-fixture serves the conformance runner's
// Sentry fake.
package main

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/packages/sentry"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

func main() {
	args := os.Args[1:]
	if sdk.PackageCommand(args, sentry.Manifest(), sentry.Certification(), os.Stdout) {
		return
	}
	if len(args) == 1 && args[0] == "certify-fixture" {
		if err := sentry.ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	_ = sdk.Serve(sentry.Build(sentry.Options{}))
}
