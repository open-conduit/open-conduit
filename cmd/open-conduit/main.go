package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alsiola/open-conduit/client"
	"github.com/alsiola/open-conduit/internal/bootstrap"
	"github.com/alsiola/open-conduit/internal/cli"
	"github.com/alsiola/open-conduit/internal/tui"
)

var version = "dev"
var commit = "unknown"

func main() {
	cwd, _ := os.Getwd()
	os.Exit(cli.RunOperator(version, commit, os.Args[1:], cli.Env{Getenv: os.Getenv, Cwd: cwd, TUI: func(c *client.Client) error { return tui.Run(c, os.Stdin, os.Stdout) }, Serve: serve}, os.Stdout, os.Stderr))
}

func serve(r cli.ServeRequest) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := bootstrap.Serve(ctx, bootstrap.Options{Version: version, Commit: commit, StateDir: r.StateDir, ConfigPath: r.ConfigPath, Socket: r.Socket, ToolbeltDir: r.ToolbeltDir, LifecycleLog: r.LifecycleLog, PollInterval: r.PollInterval, LeaseDuration: r.LeaseDuration, RetentionInterval: r.RetentionInterval})
	if err != nil {
		// Composition errors carry safe summaries (paths, codes); never config values.
		fmt.Fprintf(os.Stderr, "open-conduit: serve: %s\n", err)
		return 1
	}
	return 0
}
