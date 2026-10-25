package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	sl "github.com/alsiola/open-conduit/extensions/slack"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
)

// Slack ingress and replies (04-01). Each enabled scope's `slack` entry
// registers an ingress pair (push over Socket Mode by default, or the Events
// API on the webhook listener; and reconcile), the `slack` route starts
// `slack.conversation` Work per thread, the
// projector posts marked replies from outcomes and tool intents, and the
// slack.* operations join every generation's toolbelt. Nothing here loads a
// workspace or Git provider.

// slackSource is one registered Slack extension entry.
type slackSource struct {
	name      string
	scope     domain.ID
	config    json.RawMessage
	client    *sl.Client
	ingress   *sl.Ingress
	projector *sl.Projector
	socket    *sl.Socket // receiver socket only
	started   bool
}

type slackSources struct {
	mu      sync.Mutex
	sources map[string]*slackSource // by source namespace
	failed  map[string]string       // registration failure code by namespace
	last    time.Time
	running bool
}

// slackRegistry registers the connection type, the ingress extension type
// and (through handlers) the `slack` route handler.
func slackRegistry(reg *config.Registry) {
	if reg.Connections == nil {
		reg.Connections = map[string]func(json.RawMessage) error{}
	}
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	reg.Connections[sl.Type] = sl.ValidateConnection
	reg.Extensions[sl.Type] = config.ExtensionType{Ingress: true, Validate: sl.ValidateExtension}
}

// registerSlack registers every enabled scope's Slack entry not yet
// registered. Registrations are immutable for one runtime instance: a changed
// or removed entry keeps its registration until restart.
func (d *daemon) registerSlack(ctx context.Context, s *config.Snapshot) {
	d.slack.mu.Lock()
	defer d.slack.mu.Unlock()
	if d.slack.sources == nil {
		d.slack.sources = map[string]*slackSource{}
		d.slack.failed = map[string]string{}
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, x := range scope.Extensions {
			if x.Type != sl.Type {
				continue
			}
			settings, err := sl.ParseExtension(x.Config, x.SecretNames)
			if err != nil || settings.Scope != string(id) {
				continue
			}
			if prev, ok := d.slack.sources[settings.Namespace]; ok {
				if string(prev.config) != string(x.Config) {
					d.report("slack", domain.ID(settings.Namespace), "restart_required")
				}
				continue
			}
			if err := d.addSlack(ctx, s, id, x, settings); err != nil {
				code := errorCode(err)
				var named githubCode
				if errors.As(err, &named) {
					code = string(named)
				}
				d.report("slack", domain.ID(settings.Namespace), code)
				d.slack.failed[settings.Namespace] = code
			} else {
				delete(d.slack.failed, settings.Namespace)
			}
		}
	}
}

func (d *daemon) addSlack(ctx context.Context, s *config.Snapshot, scope domain.ID, x config.Extension, settings sl.Settings) error {
	if x.Connection == "" {
		return githubCode("slack_connection_required")
	}
	var conn *config.Connection
	for i := range s.Connections {
		if s.Connections[i].ID == x.Connection {
			conn = &s.Connections[i]
		}
	}
	if conn == nil || conn.Type != sl.Type {
		return githubCode("slack_connection_type")
	}
	cfg, err := sl.ParseConnection(conn.Config)
	if err != nil {
		return githubCode("slack_connection_invalid")
	}
	client, err := sl.NewClient(cfg, sl.SecretFunc(d.connectionSecrets(conn.ID)), sl.ClientOptions{Now: d.now})
	if err != nil {
		return err
	}
	if d.o.DryRun {
		settings.Projection.DryRun = true
	}
	in := sl.NewIngress(settings, client, d.now, sl.IngressOptions{ActiveThreads: d.activeThreads(settings)})
	interval := time.Duration(s.Settings.ReconcileSeconds) * time.Second
	name := "slack/" + string(scope) + "/" + x.Name
	if err := d.ingress.Register(ctx, name, extension.Source{Namespace: settings.Namespace, ScopeID: scope}, in, interval); err != nil {
		return err
	}
	projector, err := sl.NewProjector(sl.ProjectorOptions{Store: d.coord, Client: client, Settings: settings, Now: d.now})
	if err != nil {
		return err
	}
	src := &slackSource{name: name, scope: scope, config: append(json.RawMessage(nil), x.Config...), client: client, ingress: in, projector: projector}
	if settings.Receiver == sl.ReceiverSocket {
		src.socket = sl.NewSocket(client, sl.SocketOptions{Now: d.now})
	}
	d.slack.sources[settings.Namespace] = src
	d.log.Info("slack ingress registered", "name", name, "namespace", settings.Namespace)
	return nil
}

// activeThreads lists the threads live Work owns in an entry's namespace, so
// reconcile keeps reading their replies after the parent leaves the history
// window. It only reads durable state.
func (d *daemon) activeThreads(s sl.Settings) func(context.Context) (map[string][]string, error) {
	return func(ctx context.Context) (map[string][]string, error) {
		out := map[string][]string{}
		err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
			works, err := tx.ListWorks()
			for _, w := range works {
				if string(w.ScopeID) != s.Scope || w.State.Terminal() {
					continue
				}
				for _, k := range w.CorrelationKeys {
					if k.Source != s.Namespace {
						continue
					}
					if channel, thread, ok := sl.ParseThreadKey(k.ExternalID); ok {
						out[channel] = append(out[channel], thread)
					}
				}
			}
			return err
		})
		return out, err
	}
}

// projectSlack starts one bounded reply pass per source, at most every few
// seconds, on a single background goroutine; passes never overlap.
func (d *daemon) projectSlack(ctx context.Context) {
	d.slack.mu.Lock()
	if d.slack.running || len(d.slack.sources) == 0 || d.now().Sub(d.slack.last) < 5*time.Second {
		d.slack.mu.Unlock()
		return
	}
	d.slack.running, d.slack.last = true, d.now()
	sources := make([]*slackSource, 0, len(d.slack.sources))
	for _, src := range d.slack.sources {
		sources = append(sources, src)
	}
	d.slack.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.slack.mu.Lock()
			d.slack.running = false
			d.slack.mu.Unlock()
		}()
		for _, src := range sources {
			if ctx.Err() != nil {
				return
			}
			res, err := src.projector.Converge(ctx, passLimit)
			code := strings.Join(res.Codes, ",")
			if err != nil && code == "" {
				code = sl.ErrorCode(err)
			}
			d.report("slack_projection", domain.ID(src.name), code)
		}
	}()
}

// push hands a delivery to the runtime, which durably accepts it first.
func (d *daemon) slackPush(src *slackSource) func(context.Context, extension.Delivery) error {
	return func(ctx context.Context, delivery extension.Delivery) error {
		_, err := d.ingress.Push(ctx, src.name, delivery)
		if err == nil {
			d.poke()
		}
		return err
	}
}

// runSlackSockets starts the Socket Mode receiver of every registered entry
// that has none running yet. Receivers stop with the daemon; their
// unacknowledged envelopes are redelivered by Slack and reconcile recovers
// the rest.
func (d *daemon) runSlackSockets(ctx context.Context) {
	d.slack.mu.Lock()
	defer d.slack.mu.Unlock()
	for _, src := range d.slack.sources {
		if src.socket == nil || src.started {
			continue
		}
		src.started = true
		d.wg.Add(1)
		go func(src *slackSource) {
			defer d.wg.Done()
			src.socket.Run(ctx, d.slackPush(src))
		}(src)
	}
}

// slackSourcesList is the tools' view of the registered sources.
func (d *daemon) slackSourcesList() []sl.Source {
	d.slack.mu.Lock()
	defer d.slack.mu.Unlock()
	out := make([]sl.Source, 0, len(d.slack.sources))
	for _, s := range d.slack.sources {
		out = append(out, sl.Source{Settings: s.ingress.Settings(), Client: s.client, Projector: s.projector})
	}
	return out
}

// slackSigningSecret resolves an entry's signing_secret from its scope's
// effective references in the current generation.
func (d *daemon) slackSigningSecret(src *slackSource) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name := src.ingress.Settings().SigningSecret
		snapshot, _ := d.configs.Current()
		if name == "" || snapshot == nil || snapshot.Scopes[src.scope] == nil {
			return "", sl.ErrCredential
		}
		values, err := d.resolver.ResolveNames(ctx, snapshot.Scopes[src.scope], []string{name})
		if err != nil {
			return "", sl.ErrCredential
		}
		return values[name], nil
	}
}

// slackWebhook returns the Events API handler of the entry mounted at name
// (only receiver http entries are mounted).
func (d *daemon) slackWebhook(name string) http.Handler {
	var src *slackSource
	d.slack.mu.Lock()
	for _, s := range d.slack.sources {
		if s.name == name && s.ingress.Settings().Receiver == sl.ReceiverHTTP {
			src = s
		}
	}
	d.slack.mu.Unlock()
	if src == nil {
		return nil
	}
	return sl.WebhookHandler(d.slackSigningSecret(src), d.slackPush(src), d.now)
}

// slackWebhookItems describes each entry's webhook exposure for doctor.
func (d *daemon) slackWebhookItems() []string {
	d.slack.mu.Lock()
	defer d.slack.mu.Unlock()
	var out []string
	for ns, src := range d.slack.sources {
		switch src.ingress.Settings().Receiver {
		case sl.ReceiverHTTP:
			out = append(out, ns+": POST /"+src.name+" (set it as the app's Events API Request URL)")
		case sl.ReceiverSocket:
			out = append(out, ns+": Socket Mode (no listener needed)")
		default:
			out = append(out, ns+": receiver none (reconcile only)")
		}
	}
	return out
}

// slackCheck reports registration, rate-limit, reconcile and reply state per
// Slack entry. It makes no service calls; slack_auth tests credentials.
func (d *daemon) slackCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "slack", Status: runtime.CheckOK, Code: "ok"}
	raise := func(status, code string) {
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, code
		}
	}
	d.slack.mu.Lock()
	failed := map[string]string{}
	for ns, code := range d.slack.failed {
		failed[ns] = code
	}
	sources := map[string]*slackSource{}
	for ns, s := range d.slack.sources {
		sources[ns] = s
	}
	d.slack.mu.Unlock()
	if len(sources) == 0 && len(failed) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no Slack entries are configured"
		return check
	}
	for ns, code := range failed {
		raise(runtime.CheckFail, code)
		check.Items = append(check.Items, ns+": not registered ("+code+")")
	}
	scans := map[string]runtime.IngressStatus{}
	if statuses, err := d.ingress.Inspect(ctx); err == nil {
		for _, s := range statuses {
			scans[s.Name] = s
		}
	}
	for ns, src := range sources {
		var notes []string
		if until, paused := src.client.PausedUntil(); paused {
			raise(runtime.CheckWarn, "rate_limited")
			notes = append(notes, "rate limited until "+until.UTC().Format(time.RFC3339))
		}
		if s, ok := scans[src.name]; ok {
			switch {
			case s.LastError != "":
				raise(runtime.CheckWarn, "reconcile_failing")
				notes = append(notes, fmt.Sprintf("reconcile failing (%s, %d failures)", s.LastError, s.Failures))
			case s.LastComplete.IsZero():
				notes = append(notes, "first reconcile pending")
			default:
				notes = append(notes, "reconciled "+d.now().Sub(s.LastComplete).Truncate(time.Second).String()+" ago")
			}
		}
		if code := d.reportedCode("slack_projection", src.name); code != "" {
			raise(runtime.CheckWarn, "replies_failing")
			notes = append(notes, "replies "+code)
		}
		set := src.ingress.Settings()
		notes = append(notes, fmt.Sprintf("%d channels, start %s, receiver %s", len(set.Channels), set.Start, set.Receiver))
		if src.socket != nil {
			st := src.socket.Status()
			switch {
			case st.Connected:
				notes = append(notes, fmt.Sprintf("socket connected (%d acknowledged)", st.Acked))
			case st.LastError != "":
				raise(runtime.CheckWarn, "socket_"+st.LastError)
				notes = append(notes, "socket down ("+st.LastError+"), retrying at "+st.RetryUntil.UTC().Format(time.RFC3339))
			default:
				notes = append(notes, "socket connecting")
			}
			if st.Unacked > 0 {
				notes = append(notes, fmt.Sprintf("%d envelopes left unacknowledged for redelivery", st.Unacked))
			}
		}
		if d.o.DryRun || set.Projection.DryRun {
			notes = append(notes, "dry run (no Slack writes)")
		}
		check.Items = append(check.Items, ns+": "+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
}

// slackAuthCheck (check_secrets only) runs the extension-owned connection
// test per entry (sl.CheckAuth: team, bot user and the granted scopes
// compared with the manifest). It reports codes, never the reference or the
// token.
func (d *daemon) slackAuthCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "slack_auth", Status: runtime.CheckOK, Code: "verified"}
	d.slack.mu.Lock()
	sources := make([]*slackSource, 0, len(d.slack.sources))
	for _, s := range d.slack.sources {
		sources = append(sources, s)
	}
	d.slack.mu.Unlock()
	if len(sources) == 0 {
		check.Status, check.Code = runtime.CheckInfo, "not_configured"
		return check
	}
	for _, src := range sources {
		s := src.ingress.Settings()
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		r := sl.CheckAuth(cx, src.client, s)
		cancel()
		status := runtime.CheckOK
		switch r.Severity {
		case sl.DoctorFail:
			status = runtime.CheckFail
		case sl.DoctorWarn:
			status = runtime.CheckWarn
		}
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, r.Code
		}
		item := s.Namespace + ": " + r.Item
		check.Items = append(check.Items, item)
	}
	sort.Strings(check.Items)
	return check
}
