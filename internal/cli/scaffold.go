// Package cli currently provides help/version scaffolding only. Runtime commands
// must use the local client when stage 02 supplies it.
package cli

import (
	"fmt"
	"io"
)

// Run handles the contract-stage command surface without opening configuration,
// state, sockets or service connections. Callers supply build identity.
func Run(name, version, commit string, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintf(out, "%s — Open Conduit contracts scaffold\n\nUsage: %s [help | version]\n\nRuntime and toolbelt operations are not implemented yet.\n", name, name)
		return 0
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(out, "%s %s (%s)\n", name, version, commit)
		return 0
	}
	// Do not echo arguments: a mistaken invocation could contain credentials.
	fmt.Fprintf(errOut, "%s: command unavailable in the contracts scaffold; use --help\n", name)
	return 2
}
