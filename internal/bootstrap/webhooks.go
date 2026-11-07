package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
//   - POST /<extension>/..., served by a package's webhook.handle:
//     a schedule job's signed URL /schedule/<scope>/<entry>/<job>/<token>
//     (05-01), /slack/<scope>/<entry> for a Slack entry with receiver http
//     (04-01), and /linear|jira|bitbucket/<scope>/<entry> for tracker and
//     code-host entries (06-01);
//   - the paths an installed extension package registers at configure (07-02, 07-03).
// Anything else is 404. It never mounts /v1, never sees management
// credentials and never forwards to the management socket. An optional
// tunnel exposes only that listener; entries with manage_webhook point the
// service's webhook at it. A tunnel type (ngrok, cloudflared, or any an
// installed package declares) is provided by its package (manifest
// tunnels, extension.TunnelProvider), which checks the tunnel's config and
// runs it as written: the host holds its run open for the daemon's life,
// takes each public URL it reports, and starts it again with a growing
// wait when it stops. A static
// public URL is one the operator already routes to the listener, so nothing
// runs for it.
//
// A provider's word that the tunnel is up is not proof that anything gets
// through it (a cloudflared quick tunnel can keep running while its URL
// answers nothing), so the host checks the whole path itself: every
// tunnelProbeEvery it requests a path only its own listener answers
// (probePath and a token per daemon) through the public URL. After
// tunnelProbeFailures probes in a row fail it raises tunnel_unreachable (a
// diagnostic, a warning in the log and a telemetry event) and, for a
// provider's tunnel, ends the run so it starts again with a new URL, which
// the extensions' managed hooks then follow. A static URL is the operator's
// to fix, so the host only reports it, and keeps probing until it answers.

// TunnelOptions select webhook exposure.
type TunnelOptions struct {
	// Kind is a tunnel type a package declares, or static for PublicURL
	// ("" with PublicURL means static).
	Kind string
	// Config is the tunnel's config, opaque to the host: the providing
	// package checks it and runs it as written. Empty means {}.
	Config json.RawMessage
	// PublicURL is a URL the operator already routes to the listener.
	PublicURL string
}

// set says whether any tunnel option is given.
func (t TunnelOptions) set() bool {
	return t.Kind != "" || len(t.Config) > 0 || t.PublicURL != ""
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
	// probeToken is the path segment only this daemon's listener answers
	// under probePath; probe checks the public URL with it (probePublic,
	// or a test's).
	probeToken string
	probe      func(ctx context.Context, public, token string) error
	// probed is when a probe through the current public URL last
	// answered; probeFails counts the probes that have failed in a row
	// since, and probeCause says why the last one did.
	probed     time.Time
	probeFails int
	probeCause string
	// delivered is when a request last reached a package's webhook route.
	delivered time.Time
}

// staticTunnel is settings.webhooks' tunnel for a public URL the operator
// already routes to the listener (public_url): the host publishes it, and no
// package runs.
const staticTunnel = "static"

// tunnelBackoff is the first wait before a stopped tunnel starts again; it
// doubles to tunnelMaxBackoff and resets once a run reports a URL.
var tunnelBackoff, tunnelMaxBackoff = 5 * time.Second, 5 * time.Minute

// The probe of the public URL: one every tunnelProbeEvery, each given
// tunnelProbeTimeout, and tunnelProbeFailures in a row make the URL
// unreachable. The first probe comes a full interval after the URL does,
// so a new quick tunnel's name has time to resolve.
var (
	tunnelProbeEvery    = time.Minute
	tunnelProbeTimeout  = 15 * time.Second
	tunnelProbeFailures = 3
	// probeTicker paces the probes; tests replace it with a channel they
	// fire themselves.
	probeTicker = func(every time.Duration) (<-chan time.Time, func()) {
		t := time.NewTicker(every)
		return t.C, t.Stop
	}
)

// probePath is where the listener answers the host's own probe, followed by
// the daemon's token. No package is named ".open-conduit", so it is never a
// package's route.
const probePath = ".open-conduit/probe/"

// addTunnel registers a package's tunnel type.
func (h *webhookState) addTunnel(typ string, p extension.TunnelProvider) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.providers == nil {
		h.providers = map[string]extension.TunnelProvider{}
	}
	h.providers[typ] = p
}

// tunnelConfig is the provider's config for t: the operator's object as
// written.
func tunnelConfig(t TunnelOptions) domain.Payload {
	data := t.Config
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return domain.Payload{Schema: extension.TunnelConfigSchema, Data: data}
}

// Where the effective listener and tunnel came from, as doctor names them.
const (
	fromFlags    = "serve flags"
	fromSettings = "settings.webhooks"
)

// exposure resolves the listener address and tunnel from the serve flags
// over the stored settings.webhooks. The address and the tunnel override
// separately, and a tunnel flag replaces the whole stored tunnel, so one
// type's stored config never leaks into a run of another.
func exposure(o Options, stored config.Webhooks) (addr, addrFrom string, t TunnelOptions, tunnelFrom string) {
	addr, t = o.WebhookAddr, o.Tunnel
	if addr != "" {
		addrFrom = fromFlags
	} else if stored.Addr != "" {
		addr, addrFrom = stored.Addr, fromSettings
	}
	if t.set() {
		tunnelFrom = fromFlags
	} else if stored.Tunnel != nil || stored.PublicURL != "" {
		t = TunnelOptions{PublicURL: stored.PublicURL}
		if stored.Tunnel != nil {
			t.Kind, t.Config = stored.Tunnel.Provider, stored.Tunnel.Config
		}
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
	token := make([]byte, 16)
	_, _ = rand.Read(token)
	d.hooks.mu.Lock()
	d.hooks.probeToken = hex.EncodeToString(token)
	d.hooks.mu.Unlock()
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
		w := config.Webhooks{PublicURL: t.PublicURL}
		switch {
		case t.Kind != staticTunnel:
			w.Tunnel = &config.Tunnel{Provider: t.Kind, Config: t.Config}
		case len(t.Config) > 0:
			cancel()
			_ = srv.Close()
			return nil, errors.New("webhook tunnel (from " + tunnelFrom + "): a static public URL takes no tunnel config")
		}
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
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.watchPublic(tctx, t.Kind, nil, func(string) {})
			}()
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
		var reported, answered atomic.Bool
		var unreachable atomic.Pointer[string]
		if p == nil {
			err = errors.New("no installed or bundled package provides tunnel type " + kind)
		} else {
			d.log.Info("tunnel started", "provider", kind, "listener", run.Addr)
			// The run lasts until its URL stops answering the host's
			// probe, too: the provider's process may still be running.
			rctx, end := context.WithCancel(ctx)
			watched := make(chan struct{})
			go func() {
				defer close(watched)
				d.watchPublic(rctx, kind, &answered, func(cause string) {
					unreachable.Store(&cause)
					end()
				})
			}()
			err = p.Run(rctx, run, func(u string) {
				if u != "" {
					reported.Store(true)
				}
				d.publicURL(kind, u)
			})
			end()
			<-watched
		}
		d.publicURL(kind, "")
		if ctx.Err() != nil {
			return
		}
		cause := unreachable.Load()
		// A run whose URL never answered doesn't reset the wait, so a
		// machine that can't be reached doesn't restart its tunnel every
		// few minutes for ever.
		if answered.Load() || reported.Load() && cause == nil {
			backoff = tunnelBackoff
		}
		code := "tunnel_failed"
		switch {
		case p == nil:
			code = "tunnel_unavailable"
		case cause != nil:
			code, err = "tunnel_unreachable", errors.New(*cause)
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
		if code != "tunnel_failed" && code != "tunnel_unreachable" {
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
	if changed {
		d.hooks.probed, d.hooks.probeFails, d.hooks.probeCause = time.Time{}, 0, ""
	}
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

// watchPublic probes the public URL every tunnelProbeEvery until ctx ends.
// When tunnelProbeFailures probes in a row have failed it reports the URL
// unreachable and calls dead with why, once until the URL changes or
// answers again; a probe that answers sets answered (when given).
func (d *daemon) watchPublic(ctx context.Context, kind string, answered *atomic.Bool, dead func(cause string)) {
	tick, stop := probeTicker(tunnelProbeEvery)
	defer stop()
	var last string
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		d.hooks.mu.Lock()
		public, token, probe := d.hooks.public, d.hooks.probeToken, d.hooks.probe
		d.hooks.mu.Unlock()
		if public != last {
			last, fails = public, 0
		}
		if public == "" {
			continue
		}
		if probe == nil {
			probe = probePublic
		}
		pctx, cancel := context.WithTimeout(ctx, tunnelProbeTimeout)
		err := probe(pctx, public, token)
		cancel()
		if ctx.Err() != nil {
			return
		}
		d.hooks.mu.Lock()
		current := d.hooks.public == public
		if current && err == nil {
			d.hooks.probed, d.hooks.probeFails, d.hooks.probeCause = d.now(), 0, ""
		} else if current {
			d.hooks.probeFails, d.hooks.probeCause = fails+1, err.Error()
		}
		d.hooks.mu.Unlock()
		if !current {
			continue
		}
		if err == nil {
			if answered != nil {
				answered.Store(true)
			}
			if fails >= tunnelProbeFailures {
				d.log.Info("webhook public URL answering again", "provider", kind, "url", public)
				d.report("tunnel", domain.ID(kind), "")
				d.capture("webhooks.public_url_answering", map[string]any{"provider": kind, "url": public})
			}
			fails = 0
			continue
		}
		fails++
		if fails != tunnelProbeFailures {
			continue
		}
		cause := "public URL " + public + " is not answering: " + strconv.Itoa(fails) + " probes in a row failed (last: " + err.Error() + ")"
		attrs := []any{"provider", kind, "url", public, "probes", fails, "cause", err.Error()}
		if kind != staticTunnel {
			attrs = append(attrs, "action", "restarting the tunnel")
		}
		d.log.Warn("webhook public URL unreachable", attrs...)
		d.report("tunnel", domain.ID(kind), "tunnel_unreachable", cause)
		d.capture("webhooks.public_url_unreachable", map[string]any{"provider": kind, "url": public, "probes": fails, "cause": err.Error()})
		dead(cause)
	}
}

// probeClient sends the host's probes; a redirect is an answer from
// somewhere else, so it is never followed.
var probeClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// probePublic requests probePath with token through public and succeeds
// only when this daemon's listener answered it.
func probePublic(ctx context.Context, public, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, public+"/"+probePath+token, nil)
	if err != nil {
		return err
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("no answer within " + tunnelProbeTimeout.String())
		}
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %d", resp.StatusCode)
	}
	if string(body) != token {
		return errors.New("answered, but not from this daemon's webhook listener")
	}
	return nil
}

// capture records a host telemetry event about the machine.
func (d *daemon) capture(name string, props map[string]any) {
	if d.telemetry.primary == nil {
		return
	}
	_ = d.telemetry.Capture(context.Background(), "", name, d.now(), props)
}

// webhookHandler serves only the registered routes listed above, and the
// host's own probe.
func (d *daemon) webhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		d.hooks.mu.Lock()
		token := d.hooks.probeToken
		d.hooks.mu.Unlock()
		if token != "" && r.Method == http.MethodGet && name == probePath+token {
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, token)
			return
		}
		if h := d.packageWebhook(name); h != nil {
			d.hooks.mu.Lock()
			d.hooks.delivered = d.now()
			d.hooks.mu.Unlock()
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
	probed, probeFails, probeCause, delivered := d.hooks.probed, d.hooks.probeFails, d.hooks.probeCause, d.hooks.delivered
	listenFrom, tunnelFrom, stored := d.hooks.listenFrom, d.hooks.tunnelFrom, d.hooks.stored
	d.hooks.mu.Unlock()
	check = runtime.Check{Name: "webhooks", Status: runtime.CheckOK, Code: "listening"}
	var current *config.Snapshot
	if d.configs != nil {
		current, _ = d.configs.Current()
	}
	if current != nil && !current.Settings.Webhooks.Equal(stored) {
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
		case "tunnel_failed", "tunnel_unreachable":
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
		switch {
		case probeFails >= tunnelProbeFailures:
			check.Status, check.Code = runtime.CheckWarn, "tunnel_unreachable"
			check.Detail += "; not answering: the last " + strconv.Itoa(probeFails) + " probes through it failed (" + probeCause + "), so webhooks are not arriving"
			if kind == staticTunnel {
				check.Detail += "; check what routes it to the listener"
			} else {
				check.Detail += "; restarting the tunnel"
			}
		case probeFails > 0:
			check.Detail += "; the last probe through it failed (" + probeCause + ")"
		case !probed.IsZero():
			check.Detail += "; answered a probe at " + probed.UTC().Format(time.RFC3339)
		default:
			check.Detail += "; not probed yet"
		}
	}
	if delivered.IsZero() {
		check.Detail += "; no deliveries since serve started"
	} else {
		check.Detail += "; last delivery " + delivered.UTC().Format(time.RFC3339)
	}
	sort.Strings(check.Items)
	return check
}
