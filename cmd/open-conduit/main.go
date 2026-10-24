package main

import (
	"os"

	"github.com/alsiola/open-conduit/internal/cli"
)

var version = "dev"
var commit = "unknown"

func main() {
	os.Exit(cli.Run("open-conduit", version, commit, os.Args[1:], os.Stdout, os.Stderr))
}
