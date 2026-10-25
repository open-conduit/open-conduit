package main

import (
	"os"

	"github.com/alsiola/open-conduit/internal/cli"
)

var version = "dev"
var commit = "unknown"

func main() {
	cwd, _ := os.Getwd()
	os.Exit(cli.RunToolbelt(version, commit, os.Args[1:], cli.Env{Getenv: os.Getenv, Cwd: cwd}, os.Stdin, os.Stdout, os.Stderr))
}
