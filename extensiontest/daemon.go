// Package extensiontest runs the real Open Conduit daemon for an extension
// package's own tests: Daemon builds `open-conduit` and `os` at the version
// of this module the caller's go.mod pins, installs the package under test,
// starts `serve` and hands back a management client.
//
// Given Package and no config, it generates a scope from the package's
// manifest and a scripted agent (Agent) that calls the package's
// operations through the toolbelt, so a test reads:
//
//	d := extensiontest.Daemon(t, extensiontest.Package("."), extensiontest.Secret("api_key", "test-key"))
//	agent := d.Agent().Call("weather.forecast", map[string]any{"city": "Leeds"}).Ends("finished")
//	work := d.Start("weather.check", nil)
//	d.WaitWork(work.ID, "completed")
//	forecast := agent.Result(1, "weather.forecast")
//
// With a config of its own (Options.Config), a test writes its agent with
// FakeClaude: a scripted stand-in for the Claude Code CLI that makes
// toolbelt calls through the real `os mcp` server and ends each turn with
// a chosen state.
//
// A package repository pins the commands it builds with `tool` directives
// so the build works from the module cache:
//
//	tool (
//		github.com/open-conduit/open-conduit/cmd/open-conduit
//		github.com/open-conduit/open-conduit/cmd/os
//	)
//
// It uses only the public client and protocol packages. Daemons run on
// Unix systems only.
package extensiontest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest/devscope"
	"github.com/open-conduit/open-conduit/protocol"
)

const module = "github.com/open-conduit/open-conduit"

// Options configure a daemon.
type Options struct {
	// Config is the config file's content. Nil leaves no file, so the
	// daemon starts on its default configuration.
	Config []byte
	// Packages are package directories (each holding an
	// open-conduit-extension.json) to install under extensions/ beside
	// the config, each under its manifest's name.
	Packages []string
	// Env is added to the daemon's environment.
	Env []string
	// Args are extra `serve` flags.
	Args []string
	// PollInterval is serve's --poll-interval (50ms when zero).
	PollInterval time.Duration
	// LeaseDuration is serve's --lease-duration (2s when zero).
	LeaseDuration time.Duration
}

// Instance is a running daemon. Its directory holds config.json,
// extensions/, the state directory and daemon.log; the test's cleanup
// closes it.
type Instance struct {
	t      testing.TB
	opts   Options
	root   string
	state  string
	config string
	log    *os.File
	cmd    *exec.Cmd
	exited chan struct{}
	c      *client.Client
	starts int
	closed bool
	// started are the processes each daemon started, seen as it was
	// killed; Close checks none outlives it.
	started []devscope.Process
	// scope is the scope Daemon generated from the packages' manifests,
	// nil when the test gave its own config.
	scope *scope
	seq   int
}

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

// binaries builds open-conduit and os once per test binary, from the
// caller's module (so at the version its go.mod pins), with the race
// detector when the test runs under it.
func binaries() (string, error) {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "oc-extensiontest-bin-")
		if err != nil {
			buildErr = err
			return
		}
		for _, name := range []string{"open-conduit", "os"} {
			args := []string{"build", "-o", filepath.Join(dir, name)}
			if name == "open-conduit" && raceEnabled {
				args = append(args, "-race")
			}
			out, err := exec.Command("go", append(args, module+"/cmd/"+name)...).CombinedOutput()
			if err != nil {
				buildErr = fmt.Errorf("extensiontest: build %s: %v\n%s", name, err, out)
				return
			}
		}
		buildDir = dir
	})
	return buildDir, buildErr
}

// Daemon builds the daemon if this test binary hasn't yet, lays out its
// directory, starts it and waits until the management API answers. With
// Package and no Options.Config it writes a config of one scope for the
// packages (see Package).
func Daemon(t testing.TB, options ...Option) *Instance {
	t.Helper()
	if _, err := binaries(); err != nil {
		t.Fatal(err)
	}
	var set settings
	for _, o := range options {
		o.apply(&set)
	}
	opts := set.opts
	// Unix socket paths are short (104 bytes on macOS), and the state
	// directory holds the management socket and each run's toolbelt
	// socket, so the directory is under /tmp rather than t.TempDir().
	base := ""
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "oc-ext-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	d := &Instance{t: t, opts: opts, root: root, state: filepath.Join(root, "state"), config: filepath.Join(root, "config.json")}
	var manifests []protocol.Manifest
	for _, src := range set.packages {
		dir, m, err := buildPackage(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := install(dir, filepath.Join(root, "extensions")); err != nil {
			t.Fatal(err)
		}
		manifests = append(manifests, m)
	}
	if len(set.packages) > 0 && opts.Config == nil {
		d.generate(&set, manifests)
		opts = d.opts
	}
	if opts.Config != nil {
		if err := os.WriteFile(d.config, opts.Config, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range opts.Packages {
		if err := install(dir, filepath.Join(root, "extensions")); err != nil {
			t.Fatal(err)
		}
	}
	if d.log, err = os.Create(filepath.Join(root, "daemon.log")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if d.scope != nil {
		d.check()
	}
	d.start()
	return d
}

// install copies a package directory to extensions/<manifest name>.
func install(dir, extensions string) error {
	if err := devscope.Install(dir, extensions); err != nil {
		return fmt.Errorf("extensiontest: %w", err)
	}
	return nil
}

// Client is the operator's management client. It stays valid across
// Restart.
func (d *Instance) Client() *client.Client { return d.c }

// Root is the daemon's directory.
func (d *Instance) Root() string { return d.root }

// StateDir is serve's --data-dir.
func (d *Instance) StateDir() string { return d.state }

// ConfigPath is the config file, which may not exist (see Options.Config).
func (d *Instance) ConfigPath() string { return d.config }

// Socket is the management socket's address, for a client with another
// token.
func (d *Instance) Socket() string { return "unix://" + filepath.Join(d.state, "management.sock") }

// Log is the daemon's output so far, every start included.
func (d *Instance) Log() string {
	b, _ := os.ReadFile(filepath.Join(d.root, "daemon.log"))
	return string(b)
}

func (d *Instance) start() {
	d.t.Helper()
	dir, err := binaries()
	if err != nil {
		d.t.Fatal(err)
	}
	poll, lease := d.opts.PollInterval, d.opts.LeaseDuration
	if poll == 0 {
		poll = 50 * time.Millisecond
	}
	if lease == 0 {
		lease = 2 * time.Second
	}
	d.starts++
	fmt.Fprintf(d.log, "--- start %d\n", d.starts)
	args := append([]string{"--data-dir", d.state, "serve", "--config", d.config, "--poll-interval", poll.String(), "--lease-duration", lease.String()}, d.opts.Args...)
	cmd := exec.Command(filepath.Join(dir, "open-conduit"), args...)
	cmd.Env = append(os.Environ(), d.opts.Env...)
	cmd.Stdout, cmd.Stderr = d.log, d.log
	if err := cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	d.cmd, d.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) { _ = cmd.Wait(); close(exited) }(d.exited)
	d.Until("the daemon to answer", 30*time.Second, func() bool {
		select {
		case <-d.exited:
			d.t.Fatalf("the daemon exited at start: %v", d.cmd.ProcessState)
		default:
		}
		if d.c == nil {
			f, err := client.ReadTokenFile(filepath.Join(d.state, "private", "operator.token"))
			if err != nil {
				return false
			}
			if d.c, err = client.New(d.Socket(), f.Token); err != nil {
				return false
			}
		}
		_, err := d.c.Health(context.Background())
		return err == nil
	})
}

// Until calls fn every 25ms until it reports true, failing the test with
// what it was waiting for once timeout passes.
func (d *Instance) Until(what string, timeout time.Duration, fn func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for !fn() {
		if time.Now().After(deadline) {
			d.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Restart kills the daemon with SIGKILL and starts it again on the same
// directory. Processes it started (an agent's turn, say) are left to the
// new daemon to adopt.
func (d *Instance) Restart() {
	d.t.Helper()
	d.kill()
	d.start()
}

func (d *Instance) kill() {
	if d.cmd == nil || d.cmd.Process == nil {
		return
	}
	select {
	case <-d.exited:
		return
	default:
	}
	d.started = append(d.started, d.descendants()...)
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	<-d.exited
}

// Close kills the daemon and checks what it leaves: a race report in its
// log fails the test, as does any process a daemon started (this one, or
// one before a Restart) that is still alive a few seconds later, which
// Close then kills. The log is printed when the test failed. The test's
// cleanup calls it; calling it again does nothing.
func (d *Instance) Close() {
	if d.closed {
		return
	}
	d.closed = true
	d.kill()
	deadline := time.Now().Add(5 * time.Second)
	var left []devscope.Process
	for {
		left = devscope.Alive(d.started)
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range left {
		// An agent's wrapper leads its own process group (so it outlives
		// the daemon); the group goes with it.
		devscope.Kill(p)
		if !d.t.Failed() {
			d.t.Errorf("the daemon left process %d running: %s", p.PID, p.Command)
		}
	}
	d.log.Close()
	out := d.Log()
	if strings.Contains(out, "WARNING: DATA RACE") {
		d.t.Error("the race detector reported a data race in the daemon")
	}
	if d.t.Failed() {
		d.t.Logf("daemon log:\n%s", out)
	}
}

// descendants lists the running daemon's process tree below it.
func (d *Instance) descendants() []devscope.Process {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	select {
	case <-d.exited:
		return nil
	default:
	}
	return devscope.Descendants(d.cmd.Process.Pid)
}
