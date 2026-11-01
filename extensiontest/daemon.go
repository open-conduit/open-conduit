// Package extensiontest runs the real Open Conduit daemon for an extension
// package's own tests: Daemon builds `open-conduit` and `os` at the version
// of this module the caller's go.mod pins, installs the package under test
// beside a config, starts `serve` and hands back a management client.
// FakeClaude writes a scripted stand-in for the Claude Code CLI that makes
// toolbelt calls through the real `os mcp` server and ends each turn with a
// chosen state.
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
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
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
	started []process
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
// directory, starts it and waits until the management API answers.
func Daemon(t testing.TB, opts Options) *Instance {
	t.Helper()
	if _, err := binaries(); err != nil {
		t.Fatal(err)
	}
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
	d.start()
	return d
}

// install copies a package directory to extensions/<manifest name>.
func install(dir, extensions string) error {
	raw, err := os.ReadFile(filepath.Join(dir, protocol.ManifestFile))
	if err != nil {
		return fmt.Errorf("extensiontest: package %s: %w", dir, err)
	}
	m, err := protocol.ParseManifest(raw)
	if err != nil {
		return fmt.Errorf("extensiontest: package %s: %w", dir, err)
	}
	dst := filepath.Join(extensions, m.Name)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.CopyFS(dst, os.DirFS(dir))
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
	var left []process
	for {
		left = left[:0]
		_, running := processTable()
		for _, p := range d.started {
			// The same command at the same PID: a PID reused since is
			// someone else's.
			if running[p.pid] == p.command {
				left = append(left, p)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range left {
		// An agent's wrapper leads its own process group (so it outlives
		// the daemon); the group goes with it.
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
		if !d.t.Failed() {
			d.t.Errorf("the daemon left process %d running: %s", p.pid, p.command)
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

type process struct {
	pid     int
	command string
}

// descendants lists the running daemon's process tree below it.
func (d *Instance) descendants() []process {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	select {
	case <-d.exited:
		return nil
	default:
	}
	parent, command := processTable()
	root := d.cmd.Process.Pid
	var found []process
	for pid := range parent {
		for p, n := parent[pid], 0; p > 1 && n < 64; p, n = parent[p], n+1 {
			if p == root {
				found = append(found, process{pid, command[pid]})
				break
			}
		}
	}
	return found
}

// processTable is each running process's parent and command line.
func processTable() (parent map[int]int, command map[int]string) {
	parent, command = map[int]int{}, map[int]string{}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return parent, command
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		parent[pid], command[pid] = ppid, strings.Join(f[2:], " ")
	}
	return parent, command
}
