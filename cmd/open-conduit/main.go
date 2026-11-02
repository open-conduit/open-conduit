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
	"github.com/open-conduit/open-conduit/extensions/docker"
	"github.com/open-conduit/open-conduit/extensions/ssh"
	"github.com/open-conduit/open-conduit/internal/bootstrap"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/tui"
	"github.com/open-conduit/open-conduit/internal/web"
)

var version = "dev"
var commit = "unknown"

// updater checks for releases for upgrade and the web view's update page,
// which share its last check.
var updater = upgrade.New()

// webUpdates are the web view's release check and the service it restarts
// after an update.
func webUpdates(o web.Options) web.Options {
	o.Updates, o.Service = updater, cli.NewServiceControl(cli.SystemServiceHost(), os.Getenv)
	return o
}

func main() {
	// The hidden helper an executor's wrapper runs to start a container.
	if len(os.Args) > 2 && os.Args[1] == "container" && os.Args[2] == "run" {
		os.Exit(docker.RunHelper(os.Args[3:], docker.HelperOptions{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
	}
	// The hidden helper an executor's wrapper runs to start a run on a
	// remote over SSH.
	if len(os.Args) > 2 && os.Args[1] == "remote" && os.Args[2] == "run" {
		os.Exit(ssh.RunHelper(os.Args[3:], ssh.HelperOptions{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
	}
	cwd, _ := os.Getwd()
	env := cli.Env{Getenv: os.Getenv, Cwd: cwd, Serve: serve, ValidateConfig: validate, Setup: setup, Setups: bootstrap.Setups(), Adapters: bootstrap.Adapters}
	env.TUI = func(c *client.Client) error { return tui.Run(tui.Operator(c), os.Stdin, os.Stdout) }
	env.Web = serveWeb
	env.EditConfig = func(r cli.EditRequest) (bool, error) { return tui.EditConfig(r, os.Stdin, os.Stdout) }
	env.Migrate, env.Upgrade, env.State = bootstrap.Migrate, bootstrap.Upgrade, bootstrap.StateOps()
	// One offline validation serves a config check's warnings and its
	// diagnostics.
	check := bootstrap.NewConfigCheck()
	env.ValidateConfigAt = func(path string, data []byte) ([]client.Detail, error) { return details(check.Validate(path, data)) }
	env.Extensions = bootstrap.ExtensionOps(version, commit)
	env.ConfigWarnings = func(path string, data []byte) []string {
		warnings := append(config.Warnings(data), bootstrap.RenamedWarnings(data)...)
		warnings = append(warnings, config.IncludeWarnings(data, filepath.Dir(path))...)
		return append(warnings, check.ModeWarnings(path, data)...)
	}
	env.OfflineChecks = offlineChecks
	env.Service = cli.SystemServiceHost()
	env.Updater = updater
	os.Exit(cli.RunOperator(version, commit, os.Args[1:], env, os.Stdout, os.Stderr))
}

func serve(r cli.ServeRequest) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := bootstrap.Serve(ctx, bootstrap.Options{Version: version, Commit: commit, StateDir: r.StateDir, ConfigPath: r.ConfigPath, Socket: r.Socket, ToolbeltDir: r.ToolbeltDir, LifecycleLog: r.LifecycleLog, PollInterval: r.PollInterval, LeaseDuration: r.LeaseDuration, RetentionInterval: r.RetentionInterval, SweepInterval: r.SweepInterval,
		WebhookAddr: r.WebhookAddr, Tunnel: bootstrap.TunnelOptions{Kind: r.TunnelKind, Binary: r.TunnelBinary, Domain: r.TunnelDomain, Name: r.TunnelName, Hostname: r.TunnelHostname, PublicURL: r.PublicURL},
		NoTunnel: r.NoTunnel, DryRun: r.DryRun, ClaudeUsage: r.ClaudeUsage, NoTelemetry: r.NoTelemetry, Ready: webReady(ctx, r)})
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
	return details(bootstrap.ValidateConfigAt(path, data))
}

// details are an offline validation's diagnostics as client details.
func details(diags []config.Diagnostic, err error) ([]client.Detail, error) {
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

// serveWeb serves the browser view until interrupted, or with URLOnly
// prints its sign-in URLs.
func serveWeb(r cli.WebRequest) error {
	in, err := web.LoadSignIn(r.SignIn)
	if err != nil {
		return err
	}
	if r.URLOnly {
		urls := web.SignInURLs(in, r.Addr, r.Host)
		_, err = fmt.Fprintf(r.Out, "%s\nor, if your browser does not resolve *.localhost:\n%s\n", urls[0], urls[1])
		return err
	}
	srv, err := web.New(r.Client, webUpdates(web.Options{Addr: r.Addr, Host: r.Host, NewKey: cli.NewKey, SignIn: &in}))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx, r.Out)
}

// webReady starts serve --web's view once the daemon's socket is listening:
// a client of that socket with the operator token the daemon wrote, served
// until ctx ends. A view that cannot start says why and leaves the daemon
// running.
func webReady(ctx context.Context, r cli.ServeRequest) func() {
	if !r.Web {
		return nil
	}
	return func() {
		go func() {
			if err := runDaemonWeb(ctx, r); err != nil {
				fmt.Fprintf(os.Stderr, "open-conduit: web: %s (the daemon keeps running without it)\n", err)
			}
		}()
	}
}

func runDaemonWeb(ctx context.Context, r cli.ServeRequest) error {
	token, err := client.ReadTokenFile(filepath.Join(r.StateDir, "private", "operator.token"))
	if err != nil {
		return err
	}
	socket := r.Socket
	if socket == "" {
		socket = filepath.Join(r.StateDir, "management.sock")
	}
	c, err := client.New("unix://"+socket, token.Token)
	if err != nil {
		return err
	}
	in, err := web.LoadSignIn(r.WebSignIn)
	if err != nil {
		return err
	}
	srv, err := web.New(c, webUpdates(web.Options{Addr: r.WebAddr, Host: r.WebHost, NewKey: cli.NewKey, SignIn: &in}))
	if err != nil {
		return err
	}
	return srv.Run(ctx, os.Stderr)
}
