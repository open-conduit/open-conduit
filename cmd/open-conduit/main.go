package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/tui"
)

var version = "dev"
var commit = "unknown"

func main() {
	cwd, _ := os.Getwd()
	env := cli.Env{Getenv: os.Getenv, Cwd: cwd, Serve: serve, ValidateConfig: validate, Setup: setup, Setups: bootstrap.Setups(), Adapters: bootstrap.Adapters}
	env.TUI = func(c *client.Client) error { return tui.Run(tui.Operator(c), os.Stdin, os.Stdout) }
	env.EditConfig = func(r cli.EditRequest) (bool, error) { return tui.EditConfig(r, os.Stdin, os.Stdout) }
	env.Migrate, env.Upgrade, env.State = bootstrap.Migrate, bootstrap.Upgrade, bootstrap.StateOps()
	env.ValidateConfigAt, env.Extensions = validateAt, bootstrap.ExtensionOps(version, commit)
	env.ConfigWarnings = func(data []byte) []string { return append(config.Warnings(data), bootstrap.RenamedWarnings(data)...) }
	env.OfflineChecks = offlineChecks
	env.Service = cli.SystemServiceHost()
	os.Exit(cli.RunOperator(version, commit, os.Args[1:], env, os.Stdout, os.Stderr))
}

func serve(r cli.ServeRequest) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := bootstrap.Serve(ctx, bootstrap.Options{Version: version, Commit: commit, StateDir: r.StateDir, ConfigPath: r.ConfigPath, Socket: r.Socket, ToolbeltDir: r.ToolbeltDir, LifecycleLog: r.LifecycleLog, PollInterval: r.PollInterval, LeaseDuration: r.LeaseDuration, RetentionInterval: r.RetentionInterval, SweepInterval: r.SweepInterval,
		WebhookAddr: r.WebhookAddr, Tunnel: bootstrap.TunnelOptions{Kind: r.TunnelKind, Binary: r.TunnelBinary, Domain: r.TunnelDomain, Name: r.TunnelName, Hostname: r.TunnelHostname, PublicURL: r.PublicURL},
		NoTunnel: r.NoTunnel, DryRun: r.DryRun, ClaudeUsage: r.ClaudeUsage})
	if err != nil {
		// Composition errors carry safe summaries (paths, codes); never config values.
		fmt.Fprintf(os.Stderr, "open-conduit: serve: %s\n", err)
		return 1
	}
	return 0
}

// validate checks a config offline against the daemon's registrations.
func validate(data []byte) ([]client.Detail, error) { return validateAt("", data) }

// validateAt also knows the extension packages installed beside path.
func validateAt(path string, data []byte) ([]client.Detail, error) {
	diags, err := bootstrap.ValidateConfigAt(path, data)
	if err != nil {
		return nil, err
	}
	out := make([]client.Detail, 0, len(diags))
	for _, d := range diags {
		out = append(out, client.Detail{Path: d.Path, Code: d.Code, Message: d.Message})
	}
	return out, nil
}

// offlineChecks runs the doctor checks that need no daemon.
func offlineChecks(ctx context.Context, path string, data []byte) []client.Check {
	var out []client.Check
	for _, c := range bootstrap.OfflineChecks(ctx, path, data) {
		out = append(out, client.Check{Name: c.Name, Status: c.Status, Code: c.Code, Detail: c.Detail, Items: c.Items})
	}
	return out
}

// setup starts Claude Code with the setup guide in this terminal.
func setup(r cli.SetupRequest) int {
	cmd, err := bootstrap.SetupCommand(bootstrap.SetupOptions{Agent: r.Agent, ConfigPath: r.ConfigPath, StateDir: r.StateDir, Cwd: r.Cwd, Bin: selfCommand()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open-conduit: %s\n", err)
		return 1
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Printf("Starting Claude Code to set up Open Conduit (config %s)…\n", r.ConfigPath)
	if err := cmd.Run(); err != nil {
		return 1
	}
	return 0
}

// selfCommand is how an agent should run this binary: its name when that is
// what PATH finds, else its path.
func selfCommand() string {
	self, err := os.Executable()
	if err != nil {
		return "open-conduit"
	}
	self, _ = filepath.EvalSymlinks(self)
	if found, err := exec.LookPath("open-conduit"); err == nil {
		if f, err := filepath.EvalSymlinks(found); err == nil && f == self {
			return "open-conduit"
		}
	}
	return self
}
