package bootstrap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/extension"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/extensions/tunnel"
	"github.com/alsiola/open-conduit/internal/runtime"
)

// Webhook exposure (03-05). Webhooks are an accelerator: reconcile always
// runs, so without a listener the daemon is reconcile-only and loses only
// latency. When enabled, a separate HTTP server on its own TCP address
// serves exactly one route per registered GitHub entry that names a
// webhook_secret: POST /github/<scope>/<entry>. It never mounts /v1, never
// sees management credentials and never forwards to the management socket.
// An optional tunnel (ngrok, cloudflared or a static URL) exposes only that
// listener; entries with manage_webhook point the repository webhook at it.

// TunnelOptions select webhook exposure.
type TunnelOptions struct {
	// Kind is ngrok, cloudflared or static ("" with PublicURL means static).
	Kind, Binary, Domain, Name, Hostname, PublicURL string
}

type hookState struct {
	id, url, code string
	next          time.Time
}

type webhookState struct {
	mu        sync.Mutex
	listening string // bound address, "" when reconcile-only
	public    string // current public URL, "" while down or none
	tunnel    string // provider kind
	tunnelErr string
	hooks     map[string]*hookState // by source namespace
	managing  bool
	server    *http.Server
}

// startWebhooks binds the listener and starts the tunnel. It returns a stop
// function; with no address configured, or --no-tunnel, nothing starts.
func (d *daemon) startWebhooks(ctx context.Context) (func(), error) {
	if d.o.WebhookAddr == "" || d.o.NoTunnel {
		return func() {}, nil
	}
	ln, err := net.Listen("tcp", d.o.WebhookAddr)
	if err != nil {
		return nil, errors.New("webhook listener: cannot bind " + d.o.WebhookAddr)
	}
	srv := &http.Server{Handler: d.webhookHandler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, MaxHeaderBytes: 64 << 10}
	d.hooks.mu.Lock()
	d.hooks.listening = ln.Addr().String()
	d.hooks.server = srv
	d.hooks.hooks = map[string]*hookState{}
	d.hooks.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
	d.log.Info("webhook listener serving", "address", ln.Addr().String())
	tctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	t := d.o.Tunnel
	if t.Kind == "" && t.PublicURL != "" {
		t.Kind = "static"
	}
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

// webhookSecret resolves an entry's webhook_secret from its scope's
// effective references in the current generation.
func (d *daemon) webhookSecret(src *githubSource) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name := src.ingress.Settings().WebhookSecret
		snapshot, _ := d.configs.Current()
		if name == "" || snapshot == nil || snapshot.Scopes[src.scope] == nil {
			return "", gh.ErrCredential
		}
		values, err := d.resolver.ResolveNames(ctx, snapshot.Scopes[src.scope], []string{name})
		if err != nil {
			return "", gh.ErrCredential
		}
		return values[name], nil
	}
}

// webhookHandler serves only registered GitHub entry routes.
func (d *daemon) webhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		var src *githubSource
		d.github.mu.Lock()
		for _, s := range d.github.sources {
			if s.name == name && s.ingress.Settings().WebhookSecret != "" {
				src = s
			}
		}
		d.github.mu.Unlock()
		if src == nil {
			http.NotFound(w, r)
			return
		}
		gh.WebhookHandler(d.webhookSecret(src), func(ctx context.Context, delivery extension.Delivery) error {
			_, err := d.ingress.Push(ctx, src.name, delivery)
			if err == nil {
				d.poke()
			}
			return err
		}, d.now).ServeHTTP(w, r)
	})
}

// manageHooks points each manage_webhook entry's repository webhook at the
// current public URL, in the background, retrying a failure after a minute.
// A dry run (daemon or entry) only reports what it would do.
func (d *daemon) manageHooks(ctx context.Context) {
	d.hooks.mu.Lock()
	public := d.hooks.public
	if public == "" || d.hooks.managing {
		d.hooks.mu.Unlock()
		return
	}
	type job struct {
		src *githubSource
		url string
	}
	var jobs []job
	d.github.mu.Lock()
	for ns, src := range d.github.sources {
		s := src.ingress.Settings()
		if !s.ManageWebhook || s.WebhookSecret == "" {
			continue
		}
		st := d.hooks.hooks[ns]
		target := public + "/" + src.name
		if st != nil && (st.url == target && (st.code == "ok" || st.code == "dry_run") || d.now().Before(st.next)) {
			continue
		}
		jobs = append(jobs, job{src, target})
	}
	d.github.mu.Unlock()
	if len(jobs) == 0 {
		d.hooks.mu.Unlock()
		return
	}
	d.hooks.managing = true
	d.hooks.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.hooks.mu.Lock()
			d.hooks.managing = false
			d.hooks.mu.Unlock()
		}()
		for _, j := range jobs {
			s := j.src.ingress.Settings()
			st := &hookState{url: j.url, code: "ok"}
			d.hooks.mu.Lock()
			if prev := d.hooks.hooks[s.Namespace]; prev != nil {
				st.id = prev.id
			}
			d.hooks.mu.Unlock()
			cx, cancel := context.WithTimeout(ctx, 30*time.Second)
			secret, err := d.webhookSecret(j.src)(cx)
			if err == nil {
				dry := d.o.DryRun || s.Projection.DryRun
				var id string
				id, _, err = j.src.client.EnsureWebhook(cx, s.Repo, j.url, "/"+j.src.name, secret, st.id, dry)
				st.id = id
				if dry {
					st.code = "dry_run"
				}
			}
			cancel()
			if err != nil {
				st.code, st.next = hookCode(err), d.now().Add(time.Minute)
			}
			d.hooks.mu.Lock()
			d.hooks.hooks[s.Namespace] = st
			d.hooks.mu.Unlock()
			d.report("github_webhook", "", st.code)
		}
	}()
}

func hookCode(err error) string {
	var limit *gh.RateLimitError
	switch {
	case errors.Is(err, gh.ErrCredential):
		return "credential_unavailable"
	case errors.As(err, &limit):
		return "rate_limited"
	case gh.IsStatus(err, http.StatusNotFound), gh.IsStatus(err, http.StatusForbidden):
		return "hook_permission_denied"
	case gh.IsStatus(err, http.StatusUnauthorized):
		return "credential_rejected"
	}
	return "hook_update_failed"
}

// webhookCheck reports listener, tunnel and per-entry webhook exposure.
func (d *daemon) webhookCheck(context.Context) runtime.Check {
	d.hooks.mu.Lock()
	listening, public, kind, terr := d.hooks.listening, d.hooks.public, d.hooks.tunnel, d.hooks.tunnelErr
	hooks := map[string]hookState{}
	for k, v := range d.hooks.hooks {
		hooks[k] = *v
	}
	d.hooks.mu.Unlock()
	check := runtime.Check{Name: "webhooks", Status: runtime.CheckOK, Code: "listening"}
	if listening == "" {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "reconcile_only", "no webhook listener: events arrive through reconcile (settings.reconcile_seconds); start serve with --webhook-addr to add webhooks"
		return check
	}
	check.Detail = "listener " + listening
	switch {
	case kind == "":
		check.Detail += "; no tunnel (forward a public URL to the listener, or use --tunnel)"
	case terr != "":
		check.Status, check.Code = runtime.CheckWarn, terr
		check.Detail += "; tunnel " + kind + " stopped"
	case public == "":
		check.Status, check.Code = runtime.CheckWarn, "tunnel_down"
		check.Detail += "; tunnel " + kind + " has no public URL yet"
	default:
		check.Detail += "; public " + public + " (" + kind + ")"
	}
	d.github.mu.Lock()
	for ns, src := range d.github.sources {
		s := src.ingress.Settings()
		item := ns + ": "
		switch {
		case s.WebhookSecret == "":
			item += "no webhook_secret (not mounted; reconcile only)"
		default:
			item += "POST /" + src.name
			if h, ok := hooks[ns]; ok {
				item += "; hook " + h.code
				if h.code != "ok" && h.code != "dry_run" && check.Status == runtime.CheckOK {
					check.Status, check.Code = runtime.CheckWarn, h.code
				}
			} else if s.ManageWebhook {
				item += "; hook pending"
			}
		}
		check.Items = append(check.Items, item)
	}
	d.github.mu.Unlock()
	sort.Strings(check.Items)
	return check
}
