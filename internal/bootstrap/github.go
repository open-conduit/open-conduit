package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// githubSource is one registered GitHub extension entry: its ingress pair and
// the projector converging its tracker labels and marked comments.
type githubSource struct {
	name       string
	scope      domain.ID
	config     json.RawMessage
	connection string
	conn       string // the connection's config and secret refs the client was last set up from
	client     *gh.Client
	ingress    *gh.Ingress
	projector  *gh.Projector
}

// githubCode is a safe diagnostic code for a registration failure.
type githubCode string

func (c githubCode) Error() string { return string(c) }

type githubSources struct {
	mu          sync.Mutex
	sources     map[string]*githubSource // by source namespace
	writers     *gh.Writers              // clients of the connections bindings name (writersOnce)
	writersOnce sync.Once
	failed      map[string]string // registration failure code by namespace
	last        time.Time
	running     bool
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
// a changed or removed entry keeps its registration until restart. A change
// to the connection a registered entry uses (its sign-in, bot App, git or
// secret refs) is applied to the entry's client in place.
func (d *daemon) registerGitHub(ctx context.Context, s *config.Snapshot) {
	d.github.mu.Lock()
	defer d.github.mu.Unlock()
	if d.github.sources == nil {
		d.github.sources = map[string]*githubSource{}
		d.github.failed = map[string]string{}
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
				if string(prev.config) != string(x.Config) || prev.connection != x.Connection {
					d.report("github", domain.ID(settings.Namespace), "restart_required")
				} else {
					d.followGitHubConnection(s, settings.Namespace, prev)
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
				d.github.failed[settings.Namespace] = code
			} else {
				delete(d.github.failed, settings.Namespace)
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
	if d.o.DryRun {
		// A daemon dry run makes no GitHub writes: projections and tool
		// writes report dry_run.
		settings.Projection.DryRun = true
	}
	in := gh.NewIngress(settings, client, d.now, gh.IngressOptions{Observed: func(ctx context.Context, obs []extension.Observation) { d.tools.ObserveChecks(ctx, obs) }})
	interval := time.Duration(s.Settings.ReconcileSeconds) * time.Second
	name := "github/" + string(scope) + "/" + x.Name
	if err := d.ingress.Register(ctx, name, extension.Source{Namespace: settings.Namespace, ScopeID: scope}, in, interval); err != nil {
		return err
	}
	projector, err := gh.NewProjector(gh.ProjectorOptions{Store: d.coord, Client: client, Settings: settings, Now: d.now, Writers: d.githubWriters(), Scope: scope})
	if err != nil {
		return err
	}
	d.github.sources[settings.Namespace] = &githubSource{name: name, scope: scope, config: append(json.RawMessage(nil), x.Config...), connection: x.Connection, conn: connectionKey(conn), client: client, ingress: in, projector: projector}
	d.log.Info("github ingress registered", "name", name, "namespace", settings.Namespace)
	return nil
}

// followGitHubConnection points a registered entry's client at its
// connection's current config and secret refs when they changed since the
// client was set up, so an applied bot App or credential change takes effect
// without a restart. A move to another api_url can't be applied in place and
// reports restart_required. The caller holds d.github.mu.
func (d *daemon) followGitHubConnection(s *config.Snapshot, namespace string, src *githubSource) {
	var conn *config.Connection
	for i := range s.Connections {
		if s.Connections[i].ID == src.connection {
			conn = &s.Connections[i]
		}
	}
	if conn == nil || conn.Type != gh.Type {
		return // config validation refuses an entry without its connection
	}
	key := connectionKey(conn)
	if key == src.conn {
		return
	}
	cfg, err := gh.ParseConnection(conn.Config)
	if err == nil {
		err = src.client.Reconfigure(cfg, d.connectionSecrets(conn.ID))
	}
	if err != nil {
		d.report("github", domain.ID(namespace), "restart_required")
		return
	}
	src.conn = key
	d.log.Info("github connection change applied", "name", src.name, "connection", conn.ID)
}

// connectionKey identifies what a connection's client is built from: its
// config and secret references (names and references only, never values).
func connectionKey(c *config.Connection) string {
	refs, _ := json.Marshal(c.SecretRefs) // map keys marshal sorted
	return string(c.Config) + "\x00" + string(refs)
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

// githubWriters are the GitHub extension's clients of connections the
// scope's bindings name, resolved through the host's one rule for which
// connections an extension may act as (config.Snapshot.ExtensionConnection)
// and its secrets through the same resolver packages use (scopedSecrets).
func (d *daemon) githubWriters() *gh.Writers {
	d.github.writersOnce.Do(func() {
		types := map[string]bool{gh.Type: true}
		resolve := d.scopedSecrets(types, types)
		d.github.writers = gh.NewWriters(func(scope domain.ID, id string) (json.RawMessage, gh.SecretFunc, bool) {
			s, _ := d.extensionSnapshot()
			if s == nil {
				return nil, nil, false
			}
			c, ok := s.ExtensionConnection(scope, id, types, types)
			if !ok {
				return nil, nil, false
			}
			return c.Config, func(ctx context.Context, name string) (string, error) {
				v, err := resolve(ctx, scope, id, []string{name})
				if err != nil {
					return "", gh.ErrCredential
				}
				return v[name], nil
			}, true
		}, d.now)
	})
	return d.github.writers
}

// githubView is the GitHub extension's read-only view of the entries
// registered here, for its doctor checks: registration failures, rate-limit
// pauses, reconcile state and the last projection code.
func (d *daemon) githubView(ctx context.Context) gh.View {
	d.github.mu.Lock()
	v := gh.View{Failed: map[string]string{}}
	for ns, code := range d.github.failed {
		v.Failed[ns] = code
	}
	sources := make([]*githubSource, 0, len(d.github.sources))
	for _, s := range d.github.sources {
		sources = append(sources, s)
	}
	d.github.mu.Unlock()
	scans := map[string]runtime.IngressStatus{}
	if d.ingress != nil {
		if statuses, err := d.ingress.Inspect(ctx); err == nil {
			for _, s := range statuses {
				scans[s.Name] = s
			}
		}
	}
	for _, src := range sources {
		e := gh.ViewEntry{Name: src.name, Scope: src.scope, Connection: src.connection, Settings: src.ingress.Settings(), Projection: d.reportedCode("github_projection", src.name)}
		if until, paused := src.client.PausedUntil(); paused {
			e.PausedUntil = until
		}
		if s, ok := scans[src.name]; ok {
			e.Scan = &gh.Scan{LastError: s.LastError, Failures: s.Failures, LastComplete: s.LastComplete}
		}
		v.Entries = append(v.Entries, e)
	}
	return v
}
