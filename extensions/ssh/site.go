package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// A binding's remote is its one placement: Place gives the remote its
// config names, whatever the Work preferred, and SiteRun runs a command
// there over the same pinned connection a run uses.

const (
	// siteTimeout bounds a site command that names no timeout.
	siteTimeout = 30 * time.Minute
	// siteOutput bounds each of a site command's outputs.
	siteOutput = 16 << 20
	// sitePath is a site command's PATH unless its environment sets one.
	sitePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// Scripts for site commands, run as `sh -c <script> open-conduit <args>`.
const (
	// scriptSitePrepare <token> <socket>: the execution's toolbelt token,
	// from stdin, owner-only at its path, and the directory its socket is
	// forwarded into.
	scriptSitePrepare = `umask 077
mkdir -p "$R${1%/*}" "$R${2%/*}" || exit 3
cat > "$R$1.tmp" && mv -f "$R$1.tmp" "$R$1"`
	// scriptSiteRun <dir> <env and argv>...: the command in dir ("" for the
	// home), with an empty environment but HOME, a PATH and its own.
	scriptSiteRun = `dir=$1; shift
if [ -n "$dir" ]; then cd "$R$dir" || exit 125; fi
exec env -i HOME="$HOME" PATH=` + sitePath + ` "$@"`
)

// Place places a launch on the binding's remote, the only machine it
// names. A remote that can't be reached with its pinned key can't be
// placed on.
func (p *Provider) Place(ctx context.Context, _ extension.ResourceOwner, policy extension.ContainerPolicy, _ string) (extension.Placement, error) {
	_, s, err := ParseConfig(policy.Config.Data)
	if err != nil {
		return extension.Placement{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	if _, err := p.remote(s.Endpoint()); err != nil {
		// A placement must name one pin for its site commands to use.
		return extension.Placement{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	if h := p.Health(ctx, s, "", true); h.Status == "fail" {
		return extension.Placement{}, fmt.Errorf("%w: remote %s: %s", extension.ErrSandboxUnavailable, s.Endpoint(), h.Detail)
	}
	return extension.Placement{ID: s.Endpoint(), Daemon: s.Daemon(), Summary: "ssh " + s.Endpoint()}, nil
}

// SiteRun runs a command on a remote a binding uses. With a toolbelt, the
// execution's token is put at its path there first and its socket is
// forwarded to the same path, as for a run. The command's environment is
// its own, with HOME and a PATH unless it sets one.
func (p *Provider) SiteRun(ctx context.Context, placement string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	s, err := p.remote(placement)
	if err != nil {
		return extension.SiteResult{}, err
	}
	if len(cmd.Argv) == 0 || strings.Contains(cmd.Argv[0], "=") {
		return extension.SiteResult{}, errors.New("site command: an argv is needed, and its first word can't hold =")
	}
	if cmd.Dir != "" && !filepath.IsAbs(cmd.Dir) {
		return extension.SiteResult{}, errors.New("site command: its directory must be absolute")
	}
	for _, kv := range cmd.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || !envKey.MatchString(k) || strings.ContainsRune(kv, 0) {
			return extension.SiteResult{}, errors.New("site command: an environment entry can't be passed to a remote")
		}
	}
	timeout := cmd.Timeout
	if timeout <= 0 {
		timeout = siteTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var forward []string
	if cmd.Toolbelt != "" {
		if cmd.TokenFile == "" {
			return extension.SiteResult{}, errors.New("site command: a toolbelt needs its token file")
		}
		token, err := os.ReadFile(cmd.TokenFile)
		if err != nil {
			return extension.SiteResult{}, fmt.Errorf("site command: the toolbelt's token: %v", err)
		}
		if _, err := p.ssh(ctx, s, bytes.NewReader(token), scriptSitePrepare, cmd.TokenFile, cmd.Toolbelt); err != nil {
			return extension.SiteResult{}, fmt.Errorf("%w: %v", extension.ErrSiteUnavailable, err)
		}
		forward = []string{"-R", p.o.Root + cmd.Toolbelt + ":" + cmd.Toolbelt, "-o", "ExitOnForwardFailure=yes", "-o", "StreamLocalBindUnlink=yes"}
	}
	args := append(append([]string{cmd.Dir}, cmd.Env...), cmd.Argv...)
	argv := append(append(p.sshArgv(s), forward...), "-T", s.Host, command(p.o.Root, scriptSiteRun, args...))
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = p.cliEnv()
	c.Stdin = bytes.NewReader(cmd.Stdin)
	stdout, stderr := &bounded{max: siteOutput}, &bounded{max: siteOutput}
	c.Stdout, c.Stderr = stdout, stderr
	err = c.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit) && exit.ExitCode() != 255 && ctx.Err() == nil:
		// The command ran and failed; ssh's own failures are 255.
	default:
		return extension.SiteResult{}, fmt.Errorf("%w: %v", extension.ErrSiteUnavailable, &sshError{endpoint: s.Endpoint(), stderr: stderr.String(), hint: authHint(stderr.String(), c.Env), err: err})
	}
	return extension.SiteResult{ExitCode: c.ProcessState.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Truncated: stdout.over || stderr.over}, nil
}

// remote is the settings of the remote a placement names, among those the
// config's bindings use. Bindings that pin it to different host keys
// leave the placement without one pin, so it is unavailable, as splitRef
// refuses a run reference.
func (p *Provider) remote(placement string) (Settings, error) {
	var found *Settings
	for _, s := range p.o.Remotes() {
		if s.Endpoint() != placement {
			continue
		}
		if found != nil && found.HostKey != s.HostKey {
			return Settings{}, fmt.Errorf("%w: bindings pin %s to different host keys", extension.ErrSiteUnavailable, placement)
		}
		s := s
		found = &s
	}
	if found == nil {
		return Settings{}, fmt.Errorf("%w: no binding runs on %s", extension.ErrSiteUnavailable, placement)
	}
	return *found, nil
}

// bounded keeps the first max bytes written to it.
type bounded struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *bounded) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*bounded)(nil)
