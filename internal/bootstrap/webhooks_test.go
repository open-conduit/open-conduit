package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// exposure takes the address and the tunnel from the serve flags over
// settings.webhooks, each on its own; a tunnel flag replaces the whole
// stored tunnel, its config included.
func TestExposure(t *testing.T) {
	domain := json.RawMessage(`{"domain":"hooks.ngrok.app"}`)
	stored := config.Webhooks{Addr: "127.0.0.1:8787", Tunnel: &config.Tunnel{Provider: "ngrok", Config: domain}}
	for _, c := range []struct {
		name           string
		o              Options
		stored         config.Webhooks
		addr, addrFrom string
		tunnelFrom     string
		t              TunnelOptions
	}{
		{name: "nothing"},
		{name: "stored", stored: stored, addr: "127.0.0.1:8787", addrFrom: fromSettings, tunnelFrom: fromSettings,
			t: TunnelOptions{Kind: "ngrok", Config: domain}},
		{name: "flags only", o: Options{WebhookAddr: ":9000", Tunnel: TunnelOptions{PublicURL: "https://x.test"}}, addr: ":9000", addrFrom: fromFlags, tunnelFrom: fromFlags,
			t: TunnelOptions{Kind: "static", PublicURL: "https://x.test"}},
		{name: "address flag keeps the stored tunnel", o: Options{WebhookAddr: ":9000"}, stored: stored, addr: ":9000", addrFrom: fromFlags, tunnelFrom: fromSettings,
			t: TunnelOptions{Kind: "ngrok", Config: domain}},
		{name: "tunnel flag replaces the stored tunnel and its config", o: Options{Tunnel: TunnelOptions{Kind: "cloudflared"}}, stored: stored, addr: "127.0.0.1:8787", addrFrom: fromSettings, tunnelFrom: fromFlags,
			t: TunnelOptions{Kind: "cloudflared"}},
		{name: "an installed package's type, its config as given", o: Options{Tunnel: TunnelOptions{Kind: "tailscale-funnel", Config: json.RawMessage(`{"port": 443}`)}}, stored: stored, addr: "127.0.0.1:8787", addrFrom: fromSettings, tunnelFrom: fromFlags,
			t: TunnelOptions{Kind: "tailscale-funnel", Config: json.RawMessage(`{"port": 443}`)}},
		{name: "stored public URL means static", stored: config.Webhooks{Addr: ":1", PublicURL: "https://s.test"}, addr: ":1", addrFrom: fromSettings, tunnelFrom: fromSettings,
			t: TunnelOptions{Kind: "static", PublicURL: "https://s.test"}},
	} {
		addr, addrFrom, tun, tunnelFrom := exposure(c.o, c.stored)
		if addr != c.addr || addrFrom != c.addrFrom || tun.Kind != c.t.Kind || string(tun.Config) != string(c.t.Config) || tun.PublicURL != c.t.PublicURL || tunnelFrom != c.tunnelFrom {
			t.Errorf("%s: %q %q %+v %q", c.name, addr, addrFrom, tun, tunnelFrom)
		}
	}
}

// The provider's run gets the operator's tunnel config as written, and {}
// when there is none.
func TestTunnelConfigPassesThrough(t *testing.T) {
	raw := json.RawMessage(`{"port": 443, "tags": ["a"]}`)
	if p := tunnelConfig(TunnelOptions{Kind: "tailscale-funnel", Config: raw}); p.Schema != extension.TunnelConfigSchema || string(p.Data) != string(raw) {
		t.Fatalf("config %+v", p)
	}
	if p := tunnelConfig(TunnelOptions{Kind: "cloudflared"}); string(p.Data) != "{}" {
		t.Fatalf("no config %s", p.Data)
	}
}

// fakeTunnel is a tunnel provider whose runs follow a script: each run
// reports its URLs, then ends with its error (or waits for ctx when the
// error is errHold).
type fakeTunnel struct {
	runs []fakeRun
	n    atomic.Int32
	seen chan extension.TunnelRun
}

type fakeRun struct {
	urls []string
	err  error
}

var errHold = errors.New("hold")

func (f *fakeTunnel) Run(ctx context.Context, r extension.TunnelRun, publish func(string)) error {
	f.seen <- r
	run := f.runs[min(int(f.n.Add(1))-1, len(f.runs)-1)]
	for _, u := range run.urls {
		publish(u)
	}
	if run.err == errHold {
		<-ctx.Done()
		return nil
	}
	return run.err
}

func tunnelDaemon() *daemon {
	return &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now, reported: map[string]string{}, reportedAt: map[string]time.Time{}}
}

// The host holds the provider's run with the listener's address and the
// operator's config, takes the URL it reports, and when the run stops
// clears the URL, records the failure and starts it again; a URL from the
// new run clears the failure.
func TestSuperviseTunnelRestartsAStoppedRun(t *testing.T) {
	defer func(b time.Duration) { tunnelBackoff = b }(tunnelBackoff)
	tunnelBackoff = 0
	d := tunnelDaemon()
	f := &fakeTunnel{seen: make(chan extension.TunnelRun, 4), runs: []fakeRun{
		{urls: []string{"https://one.test/"}, err: errors.New("ngrok exited")},
		{urls: []string{"https://two.test"}, err: errHold},
	}}
	d.hooks.addTunnel("ngrok", f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	run := extension.TunnelRun{Addr: "127.0.0.1:8787", Config: tunnelConfig(TunnelOptions{Kind: "ngrok", Config: json.RawMessage(`{"domain":"hooks.ngrok.app"}`)})}
	go func() { defer close(done); d.superviseTunnel(ctx, "ngrok", run) }()
	for range 2 {
		if r := <-f.seen; r.Addr != "127.0.0.1:8787" || !strings.Contains(string(r.Config.Data), `"domain":"hooks.ngrok.app"`) {
			t.Fatalf("run %+v", r)
		}
	}
	cancel()
	<-done
	d.hooks.mu.Lock()
	defer d.hooks.mu.Unlock()
	if d.hooks.starts != 2 || d.hooks.failures != 1 || d.hooks.tunnelErr != "" || d.hooks.public != "" {
		t.Fatalf("starts %d failures %d err %q public %q", d.hooks.starts, d.hooks.failures, d.hooks.tunnelErr, d.hooks.public)
	}
}

// A config the provider refuses, or a type no package provides, is not
// started again, and doctor fails with the reason.
func TestSuperviseTunnelStopsOnARefusalOrNoProvider(t *testing.T) {
	defer func(b time.Duration) { tunnelBackoff = b }(tunnelBackoff)
	tunnelBackoff = 0
	for kind, want := range map[string]string{"ngrok": "tunnel_invalid", "cloudflared": "tunnel_unavailable"} {
		d := tunnelDaemon()
		f := &fakeTunnel{seen: make(chan extension.TunnelRun, 4), runs: []fakeRun{{err: fmt.Errorf("%w: no URL", extension.ErrTunnelConfig)}}}
		d.hooks.addTunnel("ngrok", f)
		d.hooks.listening, d.hooks.tunnel = "127.0.0.1:1", kind
		d.superviseTunnel(context.Background(), kind, extension.TunnelRun{Addr: "127.0.0.1:1"})
		c := d.webhookCheck(context.Background())
		if c.Status != runtime.CheckFail || c.Code != want {
			t.Errorf("%s: %+v", kind, c)
		}
	}
}

// A URL that can't be where webhooks point never becomes the public URL,
// whichever provider reports it.
func TestPublicURLRefusesAnUnusableURL(t *testing.T) {
	d := tunnelDaemon()
	d.publicURL("ngrok", "https://hooks.example.test/")
	d.publicURL("ngrok", "https://user:pw@evil.example.test")
	if d.hooks.public != "https://hooks.example.test" {
		t.Fatalf("public %q", d.hooks.public)
	}
}
