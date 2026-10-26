// Command weakened-extension serves one of the certification runner's
// negative controls (07-03): weakened-extension MODE.
package main

import (
	"fmt"
	"os"

	"github.com/alsiola/open-conduit/protocol/examples/weakened"
)

func main() {
	out := os.Stdout
	if len(os.Args) == 2 && os.Args[1] != "certify-fixture" {
		os.Stdout = os.Stderr
	}
	if err := weakened.Main(os.Args[1:], os.Stdin, out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
