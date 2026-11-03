package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
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
// tunnel exposes only that listener; entries with manage_webhook point the
// service's webhook at it. A tunnel type (ngrok, cloudflared) is provided by
// the package of that name (manifest tunnels, extension.TunnelProvider): the
// host holds its run open for the daemon's life, takes each public URL it
// reports, and starts it again with a growing wait when it stops. A static
// public URL is one the operator already routes to the listener, so nothing
// runs for it.

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
	stored config.Webhooks
	// tunnelErr is the code of the run's last failure ("" once it reports
	// a URL), tunnelCause its message; starts and failures count runs.
	tunnelErr, tunnelCause string
	starts, failures       int
	server                 *http.Server
	// options is the tunnel serve runs ("" Kind for none), which the
	// providing package's check sees.
	options TunnelOptions
	// providers are the packages' tunnel types (manifest tunnels).
	providers map[string]extension.TunnelProvider
}

// staticTunnel is settings.webhooks' tunnel for a public URL the operator
// already routes to the listener (public_url): the host publishes it, and no
// package runs.
const staticTunnel = "static"

// tunnelBackoff is the first wait before a stopped tunnel starts again; it
// doubles to tunnelMaxBackoff and resets once a run reports a URL.
var tunnelBackoff, tunnelMaxBackoff = 5 * time.Second, 5 * time.Minute

// addTunnel registers a package's tunnel type.
func (h *webhookState) addTunnel(typ string, p extension.TunnelProvider) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.providers == nil {
		h.providers = map[string]extension.TunnelProvider{}
	}
	h.providers[typ] = p
}

// tunnelConfig is the provider's config for t.
func tunnelConfig(t TunnelOptions) domain.Payload {
	data, _ := json.Marshal(extension.TunnelConfig{Binary: t.Binary, Domain: t.Domain, Name: t.Name, Hostname: t.Hostname})
	return domain.Payload{Schema: extension.TunnelConfigSchema, Data: data}
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
		t.Kind = staticTunnel
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
		w := config.Webhooks{Tunnel: t.Kind, TunnelDomain: t.Domain, TunnelName: t.Name, TunnelHostname: t.Hostname, PublicURL: t.PublicURL}
		if problems := w.TunnelProblems(); len(problems) > 0 {
			cancel()
			_ = srv.Close()
			return nil, errors.New("webhook tunnel (from " + tunnelFrom + "): " + problems[0].Message)
		}
		d.hooks.mu.Lock()
		d.hooks.tunnel, d.hooks.options = t.Kind, t
		d.hooks.mu.Unlock()
		if t.Kind == staticTunnel {
			// The operator already routes this URL to the listener: there
			// is nothing to run.
			d.publicURL(t.Kind, t.PublicURL)
			return stop(cancel, &wg, srv), nil
		}
		run := extension.TunnelRun{Addr: ln.Addr().String(), Config: tunnelConfig(t)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.superviseTunnel(tctx, t.Kind, run)
		}()
	}
	return stop(cancel, &wg, srv), nil
}

// stop ends the tunnel's run, then shuts the listener down.
func stop(cancel context.CancelFunc, wg *sync.WaitGroup, srv *http.Server) func() {
	return func() {
		cancel()
		wg.Wait()
		shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
	}
}

// superviseTunnel keeps the tunnel of type kind running until ctx ends: it
// starts the provider's run, takes each URL it reports, and when the run
// stops records the failure and starts it again after a growing wait. A
// config the provider refuses, or a type no package provides, is not
// started again: doctor's webhooks check says why.
func (d *daemon) superviseTunnel(ctx context.Context, kind string, run extension.TunnelRun) {
	backoff := tunnelBackoff
	for {
		d.hooks.mu.Lock()
		p := d.hooks.providers[kind]
		if p != nil {
			d.hooks.starts++
		}
		d.hooks.mu.Unlock()
		var err error
		var reported atomic.Bool
		if p == nil {
			err = errors.New("no installed or bundled package provides tunnel type " + kind)
		} else {
			d.log.Info("tunnel started", "provider", kind, "listener", run.Addr)
			err = p.Run(ctx, run, func(u string) {
				if u != "" {
					reported.Store(true)
				}
				d.publicURL(kind, u)
			})
		}
		d.publicURL(kind, "")
		if ctx.Err() != nil {
			return
		}
		if reported.Load() {
			backoff = tunnelBackoff
		}
		code := "tunnel_failed"
		switch {
		case p == nil:
			code = "tunnel_unavailable"
		case errors.Is(err, extension.ErrTunnelConfig):
			code = "tunnel_invalid"
		case err == nil:
			err = errors.New("the tunnel stopped")
		}
		d.hooks.mu.Lock()
		d.hooks.tunnelErr, d.hooks.tunnelCause = code, err.Error()
		d.hooks.failures++
		d.hooks.mu.Unlock()
		d.report("tunnel", domain.ID(kind), code, err.Error())
		if code != "tunnel_failed" {
			d.log.Error("tunnel not started", "provider", kind, "code", code, "cause", err.Error())
			return
		}
		d.log.Warn("tunnel stopped; restarting", "provider", kind, "code", code, "cause", err.Error(), "wait", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, tunnelMaxBackoff)
	}
}

// publicURL takes a URL the tunnel reported ("" while it is down). A
// change reaches the extensions' next configure through the loop.
func (d *daemon) publicURL(kind, u string) {
	u = strings.TrimRight(u, "/")
	if u != "" && extension.CheckPublicURL(u) != nil {
		// Never point webhooks at it; the wire refuses one already, and
		// this holds for any provider.
		d.log.Warn("tunnel reported an unusable public URL; ignored", "provider", kind)
		return
	}
	d.hooks.mu.Lock()
	changed := d.hooks.public != u
	d.hooks.public = u
	if u != "" {
		d.hooks.tunnelErr, d.hooks.tunnelCause = "", ""
	}
	d.hooks.mu.Unlock()
	if u != "" {
		d.report("tunnel", domain.ID(kind), "")
	}
	if changed {
		d.log.Info("webhook public URL", "provider", kind, "url", u)
		d.poke()
	}
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
	listening, public, kind, terr, cause := d.hooks.listening, d.hooks.public, d.hooks.tunnel, d.hooks.tunnelErr, d.hooks.tunnelCause
	starts, failures := d.hooks.starts, d.hooks.failures
	listenFrom, tunnelFrom, stored := d.hooks.listenFrom, d.hooks.tunnelFrom, d.hooks.stored
	d.hooks.mu.Unlock()
	check = runtime.Check{Name: "webhooks", Status: runtime.CheckOK, Code: "listening"}
	var current *config.Snapshot
	if d.configs != nil {
		current, _ = d.configs.Current()
	}
	if current != nil && current.Settings.Webhooks != stored {
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
		check.Detail += "; tunnel " + kind + " (from " + tunnelFrom + ") stopped: " + cause
		switch terr {
		case "tunnel_failed":
			check.Detail += "; restarting (" + strconv.Itoa(failures) + " stopped of " + strconv.Itoa(starts) + " started)"
		case "tunnel_unavailable":
			check.Status = runtime.CheckFail
			check.Detail += "; install a package that provides it (open-conduit extension list)"
		case "tunnel_invalid":
			check.Status = runtime.CheckFail
			check.Detail += "; fix settings.webhooks (or the serve flags) and restart serve"
		}
	case public == "":
		check.Status, check.Code = runtime.CheckWarn, "tunnel_down"
		check.Detail += "; tunnel " + kind + " (from " + tunnelFrom + ") has no public URL yet"
	default:
		check.Detail += "; public " + public + " (" + kind + ", from " + tunnelFrom + ")"
	}
	sort.Strings(check.Items)
	return check
}
