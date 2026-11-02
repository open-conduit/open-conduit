package bootstrap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/extensions/tunnel"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// Webhook exposure (03-05). Webhooks are an accelerator: reconcile always
// runs, so without a listener the daemon is reconcile-only and loses only
// latency. When enabled, a separate HTTP server on its own TCP address
// serves only the routes registered entries name, each verified by its own
// adapter before anything is accepted:
//   - POST /github/<scope>/<entry> for a GitHub entry with a webhook_secret;
//   - POST /<extension>/..., served by an in-process extension's Webhook:
//     a schedule job's signed URL /schedule/<scope>/<entry>/<job>/<token>
//     (05-01), /slack/<scope>/<entry> for a Slack entry with receiver http
//     (04-01), and /linear|jira|bitbucket/<scope>/<entry> for tracker and
//     code-host entries (06-01);
//   - the paths an installed extension package registers at configure (07-02, 07-03).
// Anything else is 404. It never mounts /v1, never sees management
// credentials and never forwards to the management socket. An optional
// tunnel (ngrok, cloudflared or a static URL) exposes only that listener;
// entries with manage_webhook point the service's webhook at it.

// TunnelOptions select webhook exposure.
type TunnelOptions struct {
	// Kind is ngrok, cloudflared or static ("" with PublicURL means static).
	Kind, Binary, Domain, Name, Hostname, PublicURL string
}

type webhookState struct {
	mu        sync.Mutex
	listening string // bound address, "" when reconcile-only
	public    string // current public URL, "" while down or none
	tunnel    string // provider kind
	// listenFrom and tunnelFrom name where the address and tunnel came from:
	// fromFlags or fromSettings ("" for no tunnel).
	listenFrom, tunnelFrom string
	// stored is the settings.webhooks the listener started from; the
	// listener never follows a reload, so doctor flags a change.
	stored    config.Webhooks
	tunnelErr string
	server    *http.Server
}

// Where the effective listener and tunnel came from, as doctor names them.
const (
	fromFlags    = "serve flags"
	fromSettings = "settings.webhooks"
)

// exposure resolves the listener address and tunnel from the serve flags
// over the stored settings.webhooks. The address and the tunnel override
// separately, and a tunnel flag replaces the whole stored tunnel, so a stored
// ngrok domain never leaks into a --tunnel cloudflared run. --tunnel-binary
// is a flag only and applies to either.
func exposure(o Options, stored config.Webhooks) (addr, addrFrom string, t TunnelOptions, tunnelFrom string) {
	addr, t = o.WebhookAddr, o.Tunnel
	if addr != "" {
		addrFrom = fromFlags
	} else if stored.Addr != "" {
		addr, addrFrom = stored.Addr, fromSettings
	}
	if t.Kind != "" || t.Domain != "" || t.Name != "" || t.Hostname != "" || t.PublicURL != "" {
		tunnelFrom = fromFlags
	} else if stored.Tunnel != "" || stored.PublicURL != "" {
		t = TunnelOptions{Kind: stored.Tunnel, Binary: t.Binary, Domain: stored.TunnelDomain, Name: stored.TunnelName, Hostname: stored.TunnelHostname, PublicURL: stored.PublicURL}
		tunnelFrom = fromSettings
	}
	if t.Kind == "" && t.PublicURL != "" {
		t.Kind = "static"
	}
	return addr, addrFrom, t, tunnelFrom
}

// bindError explains a failed webhook listener bind: why it failed, where
// the address came from, and that any free local port works.
func bindError(addr, from string, err error) error {
	var why string
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		why = "the address is already in use"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		why = "permission denied"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		why = "the address is not available on this machine"
	default:
		why = err.Error()
	}
	return errors.New("webhook listener: cannot bind " + addr + " (from " + from + "): " + why +
		"; choose another free local port with settings.webhooks.addr or --webhook-addr (for example 127.0.0.1:8788)")
}

// startWebhooks binds the listener and starts the tunnel. It returns a stop
// function; with no address in the flags or settings.webhooks, or with
// --no-tunnel, nothing starts.
func (d *daemon) startWebhooks(ctx context.Context, stored config.Webhooks) (func(), error) {
	d.hooks.mu.Lock()
	d.hooks.stored = stored
	d.hooks.mu.Unlock()
	if d.o.NoTunnel {
		return func() {}, nil
	}
	addr, addrFrom, t, tunnelFrom := exposure(d.o, stored)
	if addr == "" {
		if t.Kind != "" {
			return nil, errors.New("webhook listener: a tunnel exposes the webhook listener: add --webhook-addr (for example 127.0.0.1:8787) or settings.webhooks.addr")
		}
		return func() {}, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, bindError(addr, addrFrom, err)
	}
	srv := &http.Server{Handler: d.webhookHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	d.hooks.mu.Lock()
	d.hooks.listening = ln.Addr().String()
	d.hooks.listenFrom, d.hooks.tunnelFrom = addrFrom, tunnelFrom
	d.hooks.server = srv
	d.hooks.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	d.log.Info("webhook listener serving", "address", ln.Addr().String())
	tctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if t.Kind != "" {
		provider, err := tunnel.New(tunnel.Options{Kind: t.Kind, Binary: t.Binary, Domain: t.Domain, Name: t.Name, Hostname: t.Hostname, URL: t.PublicURL,
			Log: func(msg string, args ...any) { d.log.Warn(msg, args...) }})
		if err != nil {
			cancel()
			_ = srv.Close()
			return nil, err
		}
		d.hooks.mu.Lock()
		d.hooks.tunnel = t.Kind
		d.hooks.mu.Unlock()
		urls := make(chan string)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := provider.Run(tctx, ln.Addr().String(), urls); err != nil {
				d.hooks.mu.Lock()
				d.hooks.tunnelErr = "tunnel_failed"
				d.hooks.mu.Unlock()
				d.log.Warn("tunnel stopped", "provider", t.Kind, "code", "tunnel_failed")
			}
		}()
		go func() {
			defer wg.Done()
			for {
				select {
				case <-tctx.Done():
					return
				case u := <-urls:
					d.hooks.mu.Lock()
					changed := d.hooks.public != u
					d.hooks.public = strings.TrimRight(u, "/")
					d.hooks.mu.Unlock()
					if changed {
						d.log.Info("webhook public URL", "provider", t.Kind, "url", u)
						d.poke()
					}
				}
			}
		}()
	}
	return func() {
		cancel()
		wg.Wait()
		shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
	}, nil
}

// webhookHandler serves only the registered routes listed above.
func (d *daemon) webhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if h := d.inprocWebhook(name); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

// webhookCheck reports the listener and tunnel; each extension's check
// lists its own entries' routes and hooks.
func (d *daemon) webhookCheck(context.Context) (check runtime.Check) {
	d.hooks.mu.Lock()
	listening, public, kind, terr := d.hooks.listening, d.hooks.public, d.hooks.tunnel, d.hooks.tunnelErr
	listenFrom, tunnelFrom, stored := d.hooks.listenFrom, d.hooks.tunnelFrom, d.hooks.stored
	d.hooks.mu.Unlock()
	check = runtime.Check{Name: "webhooks", Status: runtime.CheckOK, Code: "listening"}
	if snapshot, _ := d.configs.Current(); snapshot != nil && snapshot.Settings.Webhooks != stored {
		defer func() {
			check.Status, check.Code = runtime.CheckWarn, "restart_required"
			check.Detail += "; settings.webhooks changed since serve started: restart serve to use it"
		}()
	}
	if listening == "" {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "reconcile_only", "no webhook listener: events arrive through reconcile (settings.reconcile_seconds); store settings.webhooks (or start serve with --webhook-addr) to add webhooks"
		if d.o.NoTunnel {
			check.Detail = "no webhook listener (serve --no-tunnel): events arrive through reconcile (settings.reconcile_seconds)"
		}
		return check
	}
	check.Detail = "listener " + listening + " (from " + listenFrom + ")"
	switch {
	case kind == "":
		check.Detail += "; no tunnel (forward a public URL to the listener, or set settings.webhooks.tunnel)"
	case terr != "":
		check.Status, check.Code = runtime.CheckWarn, terr
		check.Detail += "; tunnel " + kind + " (from " + tunnelFrom + ") stopped"
	case public == "":
		check.Status, check.Code = runtime.CheckWarn, "tunnel_down"
		check.Detail += "; tunnel " + kind + " (from " + tunnelFrom + ") has no public URL yet"
	default:
		check.Detail += "; public " + public + " (" + kind + ", from " + tunnelFrom + ")"
	}
	sort.Strings(check.Items)
	return check
}
