// Package tunnel exposes the host's webhook listener to the internet (ported
// from Open Supervisor's adapters/tunnel): ngrok, a Cloudflare tunnel, or a
// URL already routed to this machine. A tunnel process that exits is
// restarted with a growing wait, and each public URL it reports is passed
// on so the host can point webhooks at it. It forwards only to the webhook
// listener's address, never to the management socket.
package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Tunnel is one exposure provider.
type Tunnel interface {
	// Check reports the provider's version or target without starting it.
	Check(ctx context.Context) (string, error)
	// Run keeps the tunnel to the listener at addr (host:port) up until ctx ends,
	// sending each public URL ("" while it is down).
	Run(ctx context.Context, addr string, urls chan<- string) error
}

// Options select and configure a provider.
type Options struct {
	// Kind is ngrok, cloudflared or static.
	Kind string
	// Binary overrides the provider executable.
	Binary string
	// Domain is an ngrok reserved domain (a stable URL).
	Domain string
	// Name and Hostname select a named Cloudflare tunnel and its hostname.
	Name, Hostname string
	// URL is the static public URL.
	URL string
	// Log receives safe diagnostics.
	Log func(msg string, args ...any)
	// Backoff is the first restart wait (default 5s).
	Backoff time.Duration
}

// New returns the configured provider.
func New(o Options) (Tunnel, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Backoff <= 0 {
		o.Backoff = 5 * time.Second
	}
	switch o.Kind {
	case "ngrok":
		if o.Binary == "" {
			o.Binary = "ngrok"
		}
		return &ngrok{o}, nil
	case "cloudflared":
		if o.Binary == "" {
			o.Binary = "cloudflared"
		}
		if (o.Name == "") != (o.Hostname == "") {
			return nil, errors.New("tunnel: a named cloudflared tunnel needs both name and hostname")
		}
		return &cloudflared{o}, nil
	case "static":
		u, err := url.Parse(o.URL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return nil, errors.New("tunnel: static needs an http(s) public URL without credentials")
		}
		return &static{strings.TrimRight(o.URL, "/")}, nil
	}
	return nil, fmt.Errorf("tunnel: unknown kind %q (ngrok, cloudflared or static)", o.Kind)
}

// supervise runs a provider process until ctx ends, restarting it with a
// growing wait, and passes each output line to line, which returns a public
// URL when it sees one.
func supervise(ctx context.Context, name string, o Options, start func() *exec.Cmd, line func(string) string, urls chan<- string) error {
	backoff := o.Backoff
	for {
		cmd := start()
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = cmd.Stdout
		cmd.WaitDelay = time.Second
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("tunnel: could not start %s: %w", name, err)
		}
		stop := context.AfterFunc(ctx, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		scanned := make(chan struct{})
		go func() {
			defer close(scanned)
			scan(out, func(l string) {
				if u := line(l); u != "" {
					backoff = o.Backoff
					send(ctx, urls, u)
				}
			})
		}()
		// Wait closes the pipe once the provider exits, so a child that
		// inherited it cannot keep the scan (and the restart) waiting.
		_ = cmd.Wait()
		<-scanned
		stop()
		send(ctx, urls, "")
		if ctx.Err() != nil {
			return nil
		}
		o.Log("tunnel exited; restarting", "provider", name, "wait", backoff.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func send(ctx context.Context, urls chan<- string, u string) {
	select {
	case urls <- u:
	case <-ctx.Done():
	}
}

func scan(r io.Reader, each func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		each(sc.Text())
	}
}

type ngrok struct{ o Options }

func (n *ngrok) Check(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, n.o.Binary, "version").Output()
	if err != nil {
		return "", errors.New("ngrok version failed: install ngrok and run `ngrok config add-authtoken`")
	}
	return strings.TrimSpace(string(out)), nil
}

func (n *ngrok) Run(ctx context.Context, addr string, urls chan<- string) error {
	return supervise(ctx, "ngrok", n.o, func() *exec.Cmd {
		args := []string{"http", addr, "--log", "stdout", "--log-format", "json"}
		if n.o.Domain != "" {
			args = append(args, "--url", n.o.Domain)
		}
		return exec.Command(n.o.Binary, args...)
	}, func(l string) string {
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
			n.o.Log("ngrok reported an error")
		}
		return ""
	}, urls)
}

type cloudflared struct{ o Options }

var trycloudflare = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func (c *cloudflared) Check(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, c.o.Binary, "--version").Output()
	if err != nil {
		return "", errors.New("cloudflared --version failed: install cloudflared")
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *cloudflared) Run(ctx context.Context, addr string, urls chan<- string) error {
	target := "http://" + addr
	return supervise(ctx, "cloudflared", c.o, func() *exec.Cmd {
		if c.o.Name != "" {
			return exec.Command(c.o.Binary, "tunnel", "run", "--url", target, c.o.Name)
		}
		return exec.Command(c.o.Binary, "tunnel", "--no-autoupdate", "--url", target)
	}, func(l string) string {
		if c.o.Name != "" {
			if strings.Contains(l, "Registered tunnel connection") {
				return strings.TrimRight(c.o.Hostname, "/")
			}
			return ""
		}
		return trycloudflare.FindString(l)
	}, urls)
}

type static struct{ url string }

func (s *static) Check(context.Context) (string, error) { return "webhooks go to " + s.url, nil }

func (s *static) Run(ctx context.Context, _ string, urls chan<- string) error {
	send(ctx, urls, s.url)
	<-ctx.Done()
	return nil
}
