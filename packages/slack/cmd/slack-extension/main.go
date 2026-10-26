// Command slack-extension serves the first-party Slack extension as a
// subprocess package over protocol version 1 (07-03). Subcommands:
// package-manifest and package-certification print the package files, and
// certify-fixture serves the conformance runner's Slack fake.
package main

import (
	"fmt"
	"os"

	slackext "github.com/alsiola/open-conduit/packages/slack"
	"github.com/alsiola/open-conduit/protocol/sdk"
)

func main() {
	args := os.Args[1:]
	if sdk.PackageCommand(args, slackext.Manifest(), slackext.Certification(), os.Stdout) {
		return
	}
	if len(args) == 1 && args[0] == "certify-fixture" {
		if err := slackext.ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	_ = sdk.Serve(slackext.Build(slackext.Options{}))
}
