package bootstrap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/open-conduit/open-conduit/internal/management"
)

// terminalEnv is the only part of the daemon's environment a terminal sees.
// The daemon's environment can hold its own credentials, and an operator's
// session has no claim on them.
var terminalEnv = []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "LANG", "TMPDIR", "TZ"}

// terminalProviderEnv is what a container provider's CLI needs to reach its
// daemon or remote. It reaches only that CLI on the host, never the
// container, whose environment the provider set when it created it.
var terminalProviderEnv = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "SSH_AUTH_SOCK", "XDG_RUNTIME_DIR"}

func terminalEnviron(spec management.TerminalSpec, environ []string) []string {
	allowed := map[string]bool{}
	for _, k := range terminalEnv {
		allowed[k] = true
	}
	if spec.Contained {
		for _, k := range terminalProviderEnv {
			allowed[k] = true
		}
	}
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && (allowed[k] || strings.HasPrefix(k, "LC_")) {
			out = append(out, kv)
		}
	}
	out = append(out, spec.Env...)
	return append(out, "TERM="+spec.Term)
}

// StartTerminal runs spec under a new pseudo-terminal, in a session of its
// own, so closing it ends everything the process started there.
func StartTerminal(_ context.Context, spec management.TerminalSpec) (management.TerminalProcess, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("empty attach command")
	}
	// Not the request's context: the relay decides when this ends.
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = terminalEnviron(spec, os.Environ())
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: spec.Cols, Rows: spec.Rows})
	if err != nil {
		return nil, err
	}
	return &terminalProcess{File: f, cmd: cmd, ended: make(chan struct{})}, nil
}

// terminalHangup is how long a session has to end after its terminal hangs
// up (a session saving its transcript, say) before it is killed.
const terminalHangup = 2 * time.Second

type terminalProcess struct {
	*os.File
	cmd   *exec.Cmd
	close sync.Once
	wait  sync.Once
	ended chan struct{}
	code  int
}

func (p *terminalProcess) Resize(cols, rows uint16) error {
	return pty.Setsize(p.File, &pty.Winsize{Cols: cols, Rows: rows})
}

func (p *terminalProcess) Wait() int {
	p.wait.Do(func() {
		_ = p.cmd.Wait()
		p.code = p.cmd.ProcessState.ExitCode()
		close(p.ended)
	})
	return p.code
}

// Close ends the process's session: HUP, as a closing terminal sends, then
// KILL for whatever is still there after terminalHangup. A leader already
// reaped is left alone, since its process group ID may be reused; closing
// the terminal hangs up anything it left.
func (p *terminalProcess) Close() error {
	var err error
	p.close.Do(func() {
		go p.Wait()
		pgid := p.cmd.Process.Pid
		select {
		case <-p.ended:
			err = p.File.Close()
			return
		default:
		}
		_ = syscall.Kill(-pgid, syscall.SIGHUP)
		err = p.File.Close()
		select {
		case <-p.ended:
		//duty:terminal_hangup
		case <-time.After(terminalHangup):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-p.ended
		}
	})
	return err
}
