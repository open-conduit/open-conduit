package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extensiontest/devscope"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/web"
	"github.com/open-conduit/open-conduit/protocol"
)

// devSession is one `open-conduit dev`: a temporary directory holding the
// daemon's config, extensions and state, and the daemon running on it.
type devSession struct {
	r       cli.DevRequest
	self    string
	root    string
	state   string
	config  string
	agent   string
	webAddr string
	env     []string
	log     *os.File
	cmd     *exec.Cmd
	exited  chan struct{}
	// started are the processes each daemon started, seen as it was
	// stopped; an agent's wrapper outlives its daemon, so dev kills what
	// is left of them on exit.
	started []devscope.Process
	m       protocol.Manifest
}

// devRun is `open-conduit dev` (cli.ExtensionOps.Dev).
func devRun(ctx context.Context, r cli.DevRequest) (err error) {
	if r.Watch <= 0 {
		r.Watch = 500 * time.Millisecond
	}
	if r.Out == nil {
		r.Out = io.Discard
	}
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", r.Dir)
	}
	s := &devSession{r: r}
	if s.self, err = os.Executable(); err != nil {
		return err
	}
	if s.agent = r.Agent; s.agent == "" {
		if s.agent, err = exec.LookPath("claude"); err != nil {
			return errors.New("no claude on PATH: install Claude Code, or give the agent's binary with --agent PATH")
		}
	}
	if s.webAddr = r.WebAddr; s.webAddr == "" {
		if s.webAddr, err = freeAddr(); err != nil {
			return err
		}
	}
	// Unix socket paths are short (104 bytes on macOS), and the state
	// directory holds the management socket and each run's toolbelt
	// socket, so the directory is under /tmp rather than TMPDIR.
	base := ""
	if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
		base = "/tmp"
	}
	if s.root, err = os.MkdirTemp(base, "oc-dev-"); err != nil {
		return err
	}
	s.state, s.config = filepath.Join(s.root, "state"), filepath.Join(s.root, "config.json")
	defer func() {
		s.stop()
		s.reap()
		if s.log != nil {
			s.log.Close()
		}
		os.RemoveAll(s.root)
		if s.cmd != nil {
			fmt.Fprintln(r.Out, "Stopped the daemon and removed", s.root)
		}
	}()
	if s.log, err = os.Create(filepath.Join(s.root, "daemon.log")); err != nil {
		return err
	}
	snap := devSnapshot(r.Dir)
	if err := s.build(); err != nil {
		return err
	}
	if err := s.start(ctx); err != nil {
		return err
	}
	s.announce()
	tick := time.NewTicker(r.Watch)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		next := devSnapshot(r.Dir)
		if next.equal(snap) {
			continue
		}
		snap = next
		fmt.Fprintf(r.Out, "%s changed: rebuilding %s\n", r.Dir, s.m.Name)
		if err := s.rebuild(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(r.Out, "The rebuild failed; fix it and save again:\n%s\n", indent(err.Error()))
			continue
		}
		fmt.Fprintf(r.Out, "Restarted the daemon with %s %s\n", s.m.Name, s.m.Version)
	}
}

// build builds the package, installs it under extensions/ and writes the
// scope generated from its manifest, refusing a config that does not
// validate.
func (s *devSession) build() error {
	dir, m, err := devscope.Build(s.r.Dir)
	if err != nil {
		return err
	}
	if dir != s.r.Dir {
		defer os.RemoveAll(filepath.Dir(dir))
	}
	extensions := filepath.Join(s.root, "extensions")
	if err := os.MkdirAll(extensions, 0o755); err != nil {
		return err
	}
	if s.m.Name != "" && s.m.Name != m.Name {
		os.RemoveAll(filepath.Join(extensions, s.m.Name))
	}
	if err := devscope.Install(dir, extensions); err != nil {
		return err
	}
	o := devscope.Options{Agent: s.agent, Entries: s.r.Entries}
	// The author's own Claude Code sign-in, wherever they keep it.
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		o.AgentEnv = map[string]string{"CLAUDE_CONFIG_DIR": v}
	}
	for _, sec := range s.r.Secrets {
		o.Secrets = append(o.Secrets, devscope.Secret{Name: sec.Name, Value: sec.Value, Ref: sec.Ref})
	}
	cfg, env, err := devscope.Generate([]protocol.Manifest{m}, o)
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.config, cfg, 0o600); err != nil {
		return err
	}
	diags, err := ValidateConfigAt(s.config, cfg)
	if err != nil {
		return err
	}
	if len(diags) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "the config generated from %s's manifest is refused:\n", m.Name)
		for _, d := range diags {
			fmt.Fprintf(&b, "  %s: %s (%s)\n", d.Path, d.Message, d.Code)
		}
		fmt.Fprintf(&b, "Give the entry the config its package needs with --entry TYPE=JSON, or its secrets with --secret; %s\nThe generated config:\n%s", devscope.Docs, cfg)
		return errors.New(b.String())
	}
	s.m, s.env = m, env
	return nil
}

// rebuild builds the package again and, when that works, restarts the
// daemon on it. A failed build leaves the running daemon as it was.
func (s *devSession) rebuild(ctx context.Context) error {
	prev := s.m
	prevConfig, _ := os.ReadFile(s.config)
	if err := s.build(); err != nil {
		if prevConfig != nil {
			_ = os.WriteFile(s.config, prevConfig, 0o600)
		}
		s.m = prev
		return err
	}
	s.stop()
	return s.start(ctx)
}

// start starts the daemon and waits until the management API answers.
func (s *devSession) start(ctx context.Context) error {
	fmt.Fprintf(s.log, "--- start %s\n", time.Now().UTC().Format(time.RFC3339))
	cmd := exec.Command(s.self, "--data-dir", s.state, "serve", "--config", s.config, "--web-addr", s.webAddr)
	cmd.Env = append(os.Environ(), s.env...)
	cmd.Stdout, cmd.Stderr = s.log, s.log
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd, s.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) { _ = cmd.Wait(); close(exited) }(s.exited)
	deadline := time.Now().Add(60 * time.Second)
	var c *client.Client
	for {
		select {
		case <-s.exited:
			return fmt.Errorf("the daemon exited at start (%v); its log, %s:\n%s", cmd.ProcessState, s.log.Name(), indent(tail(s.log.Name(), 40)))
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if c == nil {
			if f, err := client.ReadTokenFile(filepath.Join(s.state, "private", "operator.token")); err == nil {
				c, _ = client.New("unix://"+filepath.Join(s.state, "management.sock"), f.Token)
			}
		}
		if c != nil {
			if _, err := c.Health(ctx); err == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon did not answer within a minute; its log is %s", s.log.Name())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop stops the daemon: SIGTERM, then SIGKILL if it is still running a
// while later. What it started is kept for reap.
func (s *devSession) stop() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	select {
	case <-s.exited:
		return
	default:
	}
	s.started = append(s.started, devscope.Descendants(s.cmd.Process.Pid)...)
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.exited
	}
}

// reap kills whatever the daemons started that is still alive a few
// seconds after the last one stopped, so dev leaves no process behind.
func (s *devSession) reap() {
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := devscope.Alive(s.started)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, p := range left {
				devscope.Kill(p)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// announce prints where the web view is and how to start a Work.
func (s *devSession) announce() {
	out := s.r.Out
	fmt.Fprintf(out, "Running %s %s from %s against a throwaway daemon\n", s.m.Name, s.m.Version, s.r.Dir)
	if in, err := web.LoadSignIn(cli.WebSignInPath(s.state)); err == nil {
		urls := web.SignInURLs(in, s.webAddr, "")
		fmt.Fprintf(out, "  Web view:   %s\n          or: %s\n", urls[0], urls[1])
	}
	for _, p := range s.m.Provides.Pages {
		fmt.Fprintf(out, "  Page:       http://%s/pages/%s (%s)\n", s.webAddr, p.ID, p.Title)
	}
	kind := "dev.try"
	if len(s.m.Provides.WorkKinds) > 0 {
		kind = s.m.Provides.WorkKinds[0].Kind
	}
	fmt.Fprintf(out, "  Start Work: open-conduit --data-dir %s work create --scope-id %s --kind %s --binding %s\n", s.state, devscope.ScopeID, kind, devscope.AgentBinding)
	fmt.Fprintf(out, "  State:      %s\n  Config:     %s\n  Daemon log: %s\n", s.state, s.config, s.log.Name())
	fmt.Fprintf(out, "Watching %s for changes; Ctrl-C stops everything.\n", s.r.Dir)
}

// freeAddr is a free port on the loopback interface.
func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// devFiles is each watched file's size and modification time.
type devFiles map[string][2]int64

func (a devFiles) equal(b devFiles) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// devSnapshot lists the package's files, leaving out hidden directories
// (.git), dependencies and build output.
func devSnapshot(dir string) devFiles {
	files := devFiles{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "dist" || d.Name() == "bin") {
				return filepath.SkipDir
			}
			return nil
		}
		if fi, err := d.Info(); err == nil {
			files[path] = [2]int64{fi.Size(), fi.ModTime().UnixNano()}
		}
		return nil
	})
	return files
}

func tail(path string, n int) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}
