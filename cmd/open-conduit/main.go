package main

import (
	"os"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/internal/cli"
	"github.com/alsiola/open-conduit/internal/tui"
)

var version = "dev"
var commit = "unknown"

func main() {
	cwd, _ := os.Getwd()
	os.Exit(cli.RunOperator(version, commit, os.Args[1:], cli.Env{Getenv: os.Getenv, Cwd: cwd, TUI: func(c *client.Client) error { return tui.Run(c, os.Stdin, os.Stdout) }}, os.Stdout, os.Stderr))
}
