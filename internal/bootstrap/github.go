package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	gh "github.com/alsiola/open-conduit/extensions/github"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/runtime"
)

// githubSource is one registered GitHub extension entry: its ingress pair and
// the projector converging its tracker labels and marked comments.
type githubSource struct {
	name      string
	config    json.RawMessage
	client    *gh.Client
	ingress   *gh.Ingress
	projector *gh.Projector
}

// githubCode is a safe diagnostic code for a registration failure.
type githubCode string

func (c githubCode) Error() string { return string(c) }

type githubSources struct {
	mu      sync.Mutex
	sources map[string]*githubSource // by source namespace
	last    time.Time
	running bool
}

// githubRegistry is the in-process registration of the GitHub extension's
// connection type, ingress extension type and `github` route handler.
func githubRegistry(reg *config.Registry) {
	if reg.Connections == nil {
		reg.Connections = map[string]func(json.RawMessage) error{}
	}
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	reg.Connections[gh.Type] = gh.ValidateConnection
	reg.Extensions[gh.Type] = config.ExtensionType{Ingress: true, Validate: gh.ValidateExtension}
}

// connectionSecrets resolves a connection's secret references lazily from the
// current generation, so rotation applies to the next credential refresh.
func (d *daemon) connectionSecrets(id string) gh.SecretFunc {
	return func(ctx context.Context, name string) (string, error) {
		snapshot, _ := d.configs.Current()
		for _, c := range snapshot.Connections {
			if c.ID == id {
				ref, ok := c.SecretRefs[name]
				if !ok || ref == "" {
					return "", gh.ErrCredential
				}
				return d.resolver.Resolve(ctx, ref)
			}
		}
		return "", gh.ErrCredential
	}
}

// registerGitHub registers every enabled scope's GitHub ingress entry that is
// not yet registered. Registrations are immutable for one runtime instance:
// a changed or removed entry keeps its registration until restart.
func (d *daemon) registerGitHub(ctx context.Context, s *config.Snapshot) {
	d.github.mu.Lock()
	defer d.github.mu.Unlock()
	if d.github.sources == nil {
		d.github.sources = map[string]*githubSource{}
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, x := range scope.Extensions {
			if x.Type != gh.Type {
				continue
			}
			settings, err := gh.ParseExtension(x.Config, x.SecretNames)
			if err != nil || settings.Scope != string(id) {
				continue // inherited entries for another scope are validated elsewhere
			}
			if prev, ok := d.github.sources[settings.Namespace]; ok {
				if string(prev.config) != string(x.Config) {
					d.report("github", domain.ID(settings.Namespace), "restart_required")
				}
				continue
			}
			if err := d.addGitHub(ctx, s, id, x, settings); err != nil {
				code := errorCode(err)
				var named githubCode
				if errors.As(err, &named) {
					code = string(named)
				}
				d.report("github", domain.ID(settings.Namespace), code)
			}
		}
	}
}

func (d *daemon) addGitHub(ctx context.Context, s *config.Snapshot, scope domain.ID, x config.Extension, settings gh.Settings) error {
	if x.Connection == "" {
		return githubCode("github_connection_required")
	}
	var conn *config.Connection
	for i := range s.Connections {
		if s.Connections[i].ID == x.Connection {
			conn = &s.Connections[i]
		}
	}
	if conn == nil || conn.Type != gh.Type {
		return githubCode("github_connection_type")
	}
	cfg, err := gh.ParseConnection(conn.Config)
	if err != nil {
		return githubCode("github_connection_invalid")
	}
	client, err := gh.NewClient(cfg, d.connectionSecrets(conn.ID), gh.ClientOptions{Now: d.now})
	if err != nil {
		return err
	}
	in := gh.NewIngress(settings, client, d.now, gh.IngressOptions{})
	interval := time.Duration(s.Settings.ReconcileSeconds) * time.Second
	name := "github/" + string(scope) + "/" + x.Name
	if err := d.ingress.Register(ctx, name, extension.Source{Namespace: settings.Namespace, ScopeID: scope}, in, interval); err != nil {
		return err
	}
	projector, err := gh.NewProjector(gh.ProjectorOptions{Store: d.coord, Client: client, Settings: settings, Now: d.now})
	if err != nil {
		return err
	}
	d.github.sources[settings.Namespace] = &githubSource{name: name, config: append(json.RawMessage(nil), x.Config...), client: client, ingress: in, projector: projector}
	d.log.Info("github ingress registered", "name", name, "namespace", settings.Namespace)
	return nil
}

// projectGitHub starts one bounded projection pass per source, at most every
// few seconds, on a single background goroutine: projections make network
// calls and must never stall routing or starts. Passes never overlap; they
// are idempotent and level-triggered.
func (d *daemon) projectGitHub(ctx context.Context) {
	d.github.mu.Lock()
	if d.github.running || len(d.github.sources) == 0 || d.now().Sub(d.github.last) < 5*time.Second {
		d.github.mu.Unlock()
		return
	}
	d.github.running, d.github.last = true, d.now()
	sources := make([]*githubSource, 0, len(d.github.sources))
	for _, src := range d.github.sources {
		sources = append(sources, src)
	}
	d.github.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.github.mu.Lock()
			d.github.running = false
			d.github.mu.Unlock()
		}()
		for _, src := range sources {
			if ctx.Err() != nil {
				return
			}
			res, err := src.projector.Converge(ctx, passLimit)
			code := strings.Join(res.Codes, ",")
			if err != nil && code == "" {
				code = errorCode(err)
			}
			d.report("github_projection", domain.ID(src.name), code)
		}
	}()
}

// githubCheck reports rate-limit pauses for doctor.
func (d *daemon) githubCheck(context.Context) runtime.Check {
	d.github.mu.Lock()
	defer d.github.mu.Unlock()
	check := runtime.Check{Name: "github", Status: "ok", Code: "ok"}
	if len(d.github.sources) == 0 {
		check.Status, check.Code = "info", "not_configured"
		return check
	}
	for ns, src := range d.github.sources {
		if until, paused := src.client.PausedUntil(); paused {
			check.Status, check.Code = "warn", "rate_limited"
			check.Items = append(check.Items, ns+" until "+until.UTC().Format(time.RFC3339))
		}
	}
	return check
}
