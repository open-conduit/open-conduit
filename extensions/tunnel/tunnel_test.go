package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func next(t *testing.T, urls <-chan string) string {
	t.Helper()
	select {
	case u := <-urls:
		return u
	case <-time.After(10 * time.Second):
		t.Fatal("no URL reported")
	}
	return ""
}

func provider(t *testing.T, kind string) *Provider {
	t.Helper()
	p, err := New(kind)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// run starts one Run and returns the URLs it publishes and its result.
func run(ctx context.Context, p *Provider, addr string, cfg Config) (<-chan string, <-chan error) {
	urls, done := make(chan string, 8), make(chan error, 1)
	go func() {
		done <- p.Run(ctx, extension.TunnelRun{Addr: addr, Config: Payload(cfg)}, func(u string) { urls <- u })
	}()
	return urls, done
}

// ngrok's JSON log announces the URL; when the process exits, the run
// publishes "" and ends with an error, so the host decides on a restart.
func TestNgrokReportsURLThenEndsWhenItExits(t *testing.T) {
	dir := t.TempDir()
	bin := script(t, `case "$1" in version) echo "ngrok version 3.0.0"; exit 0 ;; esac
echo "$@" >> `+dir+`/args
echo '{"lvl":"info","msg":"started tunnel","url":"https://abc.ngrok.app"}'
sleep 0.2
`)
	p := provider(t, Ngrok)
	cfg := Config{Binary: bin, Domain: "abc.ngrok.app"}
	if v, err := p.Check(context.Background(), Payload(cfg).Data); err != nil || !strings.Contains(v, "3.0.0") {
		t.Fatalf("check: %q %v", v, err)
	}
	urls, done := run(context.Background(), p, "127.0.0.1:8787", cfg)
	for _, want := range []string{"https://abc.ngrok.app", ""} {
		if got := next(t, urls); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "ngrok exited") {
		t.Fatalf("run ended with %v", err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "http 127.0.0.1:8787") || !strings.Contains(string(args), "--url abc.ngrok.app") {
		t.Fatalf("ngrok args: %s", args)
	}
}

// Cancelling the run kills the process and ends it cleanly.
func TestCloudflaredQuickAndNamed(t *testing.T) {
	quick := script(t, `echo "INF +----+"; echo "INF |  https://brave-fox-12.trycloudflare.com  |"; sleep 5`)
	ctx, cancel := context.WithCancel(context.Background())
	urls, done := run(ctx, provider(t, Cloudflared), "127.0.0.1:1", Config{Binary: quick})
	if got := next(t, urls); got != "https://brave-fox-12.trycloudflare.com" {
		t.Fatalf("quick: %q", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("cancelled run ended with %v", err)
	}
	named := script(t, `echo "INF Registered tunnel connection connIndex=0"; sleep 5`)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	urls, _ = run(ctx, provider(t, Cloudflared), "127.0.0.1:1", Config{Binary: named, Name: "hooks", Hostname: "https://hooks.example.com/"})
	if got := next(t, urls); got != "https://hooks.example.com" {
		t.Fatalf("named: %q", got)
	}
}

func TestInvalid(t *testing.T) {
	for kind, cfg := range map[string]Config{
		Cloudflared:             {Name: "x"},
		Cloudflared + " domain": {Domain: "x"},
		Ngrok:                   {Hostname: "h"},
	} {
		typ, _, _ := strings.Cut(kind, " ")
		_, done := run(context.Background(), provider(t, typ), "127.0.0.1:1", cfg)
		if err := <-done; !errors.Is(err, extension.ErrTunnelConfig) {
			t.Fatalf("%s %+v: %v", kind, cfg, err)
		}
	}
	if _, err := New("static"); err == nil {
		t.Fatal("unknown type accepted")
	}
}

// Validate is the package's check of an operator's tunnel config (validate
// kind tunnel): each type takes its own fields and nothing else, and a
// refusal names the rule, never a value.
func TestValidate(t *testing.T) {
	for _, c := range []struct {
		kind, config string
		ok           bool
	}{
		{Ngrok, ``, true},
		{Ngrok, `{}`, true},
		{Ngrok, `{"domain": "hooks.ngrok.app", "binary": "/opt/ngrok"}`, true},
		{Ngrok, `{"name": "hooks"}`, false},
		{Ngrok, `{"domian": "hooks.ngrok.app"}`, false},
		{Ngrok, `"hooks.ngrok.app"`, false},
		{Cloudflared, `{}`, true},
		{Cloudflared, `{"name": "hooks", "hostname": "https://hooks.example.com"}`, true},
		{Cloudflared, `{"name": "hooks"}`, false},
		{Cloudflared, `{"name": "hooks", "hostname": "hooks.example.com"}`, false},
		{Cloudflared, `{"domain": "secret-domain.example"}`, false},
	} {
		err := provider(t, c.kind).Validate(json.RawMessage(c.config))
		if (err == nil) != c.ok {
			t.Errorf("%s %s: %v, want ok=%t", c.kind, c.config, err, c.ok)
		}
		if err != nil && (!errors.Is(err, extension.ErrTunnelConfig) || strings.Contains(err.Error(), "secret-domain") || strings.Contains(err.Error(), "domian")) {
			t.Errorf("%s %s: refusal %q is not a fixed reason", c.kind, c.config, err)
		}
	}
}
