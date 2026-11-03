// Package tunnel exposes the host's webhook listener to the internet (ported
// from Open Supervisor's adapters/tunnel) through ngrok or a Cloudflare
// tunnel. Each type is an extension.TunnelProvider: one Run starts the provider's process, publishes
// each public URL it reports, and returns when the process exits, so the
// host decides when to start it again. It forwards only to the listener's
// address the host gives it, never to the management socket.
package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// The tunnel types this package provides, each served as its own package
// of the same name.
const (
	Ngrok       = "ngrok"
	Cloudflared = "cloudflared"
)

// Provider is one tunnel type.
type Provider struct {
	kind string
	// Log receives safe diagnostics (never a URL's credentials or a token).
	Log func(msg string, args ...any)

	mu      sync.Mutex
	running map[*exec.Cmd]bool
}

// Close kills every provider process still running. A package calls it as
// it exits, which can be before a cancelled run has stopped its process.
func (p *Provider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for cmd := range p.running {
		_ = cmd.Process.Kill()
	}
}

// New returns the provider of a type.
func New(kind string) (*Provider, error) {
	switch kind {
	case Ngrok, Cloudflared:
		return &Provider{kind: kind, Log: func(string, ...any) {}, running: map[*exec.Cmd]bool{}}, nil
	}
	return nil, fmt.Errorf("tunnel: unknown type %q (ngrok or cloudflared)", kind)
}

// Config returns cfg as the payload a provider runs with.
func Config(cfg extension.TunnelConfig) domain.Payload {
	data, _ := json.Marshal(cfg)
	return domain.Payload{Schema: extension.TunnelConfigSchema, Data: data}
}

// parse reads and checks a run's config for the provider's type.
func (p *Provider) parse(raw json.RawMessage) (extension.TunnelConfig, error) {
	var c extension.TunnelConfig
	if len(bytes.TrimSpace(raw)) > 0 {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			return c, fmt.Errorf("%w: %v", extension.ErrTunnelConfig, err)
		}
	}
	invalid := func(msg string) (extension.TunnelConfig, error) {
		return c, fmt.Errorf("%w: %s", extension.ErrTunnelConfig, msg)
	}
	switch p.kind {
	case Ngrok:
		if c.Name != "" || c.Hostname != "" {
			return invalid("ngrok takes a binary and a domain")
		}
		if c.Binary == "" {
			c.Binary = "ngrok"
		}
	case Cloudflared:
		if c.Domain != "" {
			return invalid("cloudflared takes a binary, and a name and hostname for a named tunnel")
		}
		if (c.Name == "") != (c.Hostname == "") {
			return invalid("a named cloudflared tunnel needs both name and hostname")
		}
		if c.Binary == "" {
			c.Binary = "cloudflared"
		}
	}
	return c, nil
}

// Check reports the provider's version without starting it.
func (p *Provider) Check(ctx context.Context, raw json.RawMessage) (string, error) {
	c, err := p.parse(raw)
	if err != nil {
		return "", err
	}
	switch p.kind {
	case Ngrok:
		out, err := exec.CommandContext(ctx, c.Binary, "version").Output()
		if err != nil {
			return "", errors.New("ngrok version failed: install ngrok and run `ngrok config add-authtoken`")
		}
		return strings.TrimSpace(string(out)), nil
	}
	out, err := exec.CommandContext(ctx, c.Binary, "--version").Output()
	if err != nil {
		return "", errors.New("cloudflared --version failed: install cloudflared")
	}
	return strings.TrimSpace(string(out)), nil
}

// Run keeps the tunnel to r.Addr up until ctx ends or the provider's
// process exits (extension.TunnelProvider).
func (p *Provider) Run(ctx context.Context, r extension.TunnelRun, publish func(string)) error {
	c, err := p.parse(r.Config.Data)
	if err != nil {
		return err
	}
	if p.kind == Ngrok {
		args := []string{"http", r.Addr, "--log", "stdout", "--log-format", "json"}
		if c.Domain != "" {
			args = append(args, "--url", c.Domain)
		}
		return p.process(ctx, exec.Command(c.Binary, args...), func(l string) string {
			var e struct {
				Msg, URL, Lvl, Err string
			}
			if json.Unmarshal([]byte(l), &e) != nil {
				return ""
			}
			if e.Msg == "started tunnel" && e.URL != "" {
				return e.URL
			}
			if e.Lvl == "eror" || e.Lvl == "error" {
				p.Log("ngrok reported an error")
			}
			return ""
		}, publish)
	}
	target := "http://" + r.Addr
	cmd := exec.Command(c.Binary, "tunnel", "--no-autoupdate", "--url", target)
	if c.Name != "" {
		cmd = exec.Command(c.Binary, "tunnel", "run", "--url", target, c.Name)
	}
	return p.process(ctx, cmd, func(l string) string {
		if c.Name != "" {
			if strings.Contains(l, "Registered tunnel connection") {
				return strings.TrimRight(c.Hostname, "/")
			}
			return ""
		}
		return trycloudflare.FindString(l)
	}, publish)
}

var trycloudflare = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// process runs the provider's process until it exits or ctx ends, passing
// each output line to line, which returns a public URL when it sees one.
// The process is killed when ctx ends; an exit before that is an error.
func (p *Provider) process(ctx context.Context, cmd *exec.Cmd, line func(string) string, publish func(string)) error {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	cmd.WaitDelay = time.Second
	p.mu.Lock()
	err = cmd.Start()
	if err == nil {
		p.running[cmd] = true
	}
	p.mu.Unlock()
	if err != nil {
		return fmt.Errorf("tunnel: could not start %s: %w", p.kind, err)
	}
	defer func() {
		p.mu.Lock()
		delete(p.running, cmd)
		p.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	defer stop()
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		scan(out, func(l string) {
			if u := line(l); u != "" {
				publish(u)
			}
		})
	}()
	// Wait closes the pipe once the provider exits, so a child that
	// inherited it cannot keep the scan waiting.
	werr := cmd.Wait()
	<-scanned
	publish("")
	if ctx.Err() != nil {
		return nil
	}
	if werr != nil {
		return fmt.Errorf("tunnel: %s exited: %w", p.kind, werr)
	}
	return fmt.Errorf("tunnel: %s exited", p.kind)
}

func scan(r io.Reader, each func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		each(sc.Text())
	}
}
