// Package slackext packages the first-party Slack extension
// (extensions/slack, unchanged) as an installable subprocess extension
// (07-03): the same ingress, route, tools, projector, socket receiver,
// Events API webhook, doctor checks and setup, served over protocol
// version 1 through protocol/sdk. It is one of the two first-party
// workloads round-tripped through the subprocess boundary; the daemon keeps
// registering Slack in-process, so this package cannot be installed beside
// it (its names conflict) and exists to prove and certify the boundary.
//
// What changes across the boundary:
//   - KV and state reads go through sdk.Store (the extension's own
//     namespaces with precondition commits; scoped state reads);
//   - the socket receiver delivers through ingress.deliver and acknowledges
//     Slack only after the host's durable acceptance;
//   - the active-thread filter, which bootstrap computes in-process, is a
//     state.works read here (07-01's note);
//   - secrets are resolved per connection through secret.resolve;
//   - source namespaces must be "slack" or begin with "slack/", the
//     subprocess namespacing rule.
package slackext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	sl "github.com/alsiola/open-conduit/extensions/slack"
	"github.com/alsiola/open-conduit/protocol"
	"github.com/alsiola/open-conduit/protocol/sdk"
	"github.com/alsiola/open-conduit/storage"
)

// Name is the package's registration name: the first-party Slack name, so
// families, schemas, operations and config types are identical.
const Name = sl.Type

// Version is this package build's own version.
const Version = "0.1.0"

// ProjectTask is the background projection pass.
const ProjectTask = "slack-project"

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	info := sl.Setup{}.SetupInfo()
	p := protocol.Provides{
		Connections: []protocol.TypeDecl{{Type: sl.Type}},
		Extensions:  []protocol.ExtensionDecl{{Type: sl.Type, Ingress: true}},
		Schemas:     []protocol.SchemaDecl{{Name: "slack.message", Versions: []int{1}}, {Name: "slack.message.removed", Versions: []int{1}}},
		Routes:      []protocol.RouteDecl{{Handler: sl.Type}},
		WorkKinds:   []protocol.WorkKindDecl{{Kind: sl.WorkKind}},
		Tasks:       []protocol.TaskDecl{{Name: ProjectTask, IntervalSeconds: 5}},
		Webhooks:    true,
		Checks:      []protocol.CheckDecl{{Name: "slack"}, {Name: "slack-auth", Deep: true}},
		Setup:       &protocol.SetupDecl{Type: info.Type, Summary: info.Summary, Usage: info.Usage},
		Render:      true,
	}
	for _, f := range sl.Families() {
		p.Families = append(p.Families, protocol.Family{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, op := range sl.Operations() {
		p.Operations = append(p.Operations, protocol.OperationDecl{Name: op, Kind: protocol.OperationHandler})
	}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version,
		Summary: "The first-party Slack extension (workspace-free support threads) as a subprocess package.",
		Command: []string{"slack-extension"}, Provides: p}
}

// Options configure Build.
type Options struct {
	Now func() time.Time
	// HTTP overrides the Slack client's HTTP client (tests).
	HTTP *http.Client
}

type source struct {
	name, scope, config string
	settings            sl.Settings
	client              *sl.Client
	ingress             *sl.Ingress
	projector           *sl.Projector
	stop                func()
}

type state struct {
	o       Options
	host    atomic.Pointer[sdk.Host] // the connection's, from the first configure
	store   storage.Store
	mu      sync.Mutex
	sources map[string]*source // by registration name
}

// lazyStore is sdk.Store over the connection's host, known from the first
// configure (nothing reads state before it).
type lazyStore struct{ host func() *sdk.Host }

func (s lazyStore) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return sdk.NewStore(s.host()).Transact(ctx, g, fn)
}

func (st *state) list() []*source {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*source, 0, len(st.sources))
	for _, s := range st.sources {
		out = append(out, s)
	}
	return out
}

// ValidNamespace is the subprocess rule for Slack source namespaces.
func ValidNamespace(ns string) bool { return protocol.SourceNamespace(Name, ns) }

// Build returns the served extension.
func Build(o Options) *sdk.Extension {
	if o.Now == nil {
		o.Now = time.Now
	}
	st := &state{o: o, sources: map[string]*source{}}
	st.store = lazyStore{st.host.Load}
	tools, _ := sl.NewTools(sl.ToolsOptions{Store: st.store, Now: o.Now, Sources: func() []sl.Source {
		var out []sl.Source
		for _, s := range st.list() {
			out = append(out, sl.Source{Settings: s.settings, Client: s.client, Projector: s.projector})
		}
		return out
	}})
	ext := &sdk.Extension{Manifest: Manifest()}
	ext.Validate = func(_ context.Context, p protocol.ValidateParams) error {
		switch p.Kind {
		case protocol.ValidateConnection:
			return sl.ValidateConnection(p.Config)
		case protocol.ValidateExtension:
			s, err := sl.ParseExtension(p.Config, p.SecretNames)
			if err != nil {
				return err
			}
			if !ValidNamespace(s.Namespace) {
				return errors.New("a packaged Slack namespace must be slack or begin with slack/")
			}
		}
		return nil
	}
	ext.Configure = st.configure
	ext.Ingress = func(name string) (extension.Ingress, bool) {
		st.mu.Lock()
		defer st.mu.Unlock()
		s, ok := st.sources[name]
		if !ok {
			return nil, false
		}
		return s.ingress, true
	}
	ext.Routes = map[string]extension.RouteHandler{sl.Type: sl.Route{}}
	ext.Operations = tools.Registration().Operations
	ext.Tasks = map[string]func(context.Context, *sdk.Host) error{ProjectTask: func(ctx context.Context, _ *sdk.Host) error {
		var errs []error
		for _, s := range st.list() {
			if _, err := s.projector.Converge(ctx, 100); err != nil {
				errs = append(errs, fmt.Errorf("%s: %s", s.name, sl.ErrorCode(err)))
			}
		}
		return errors.Join(errs...)
	}}
	ext.Render = sl.RenderEvent
	ext.Checks = map[string]func(context.Context, *sdk.Host, bool) (string, error){
		"slack": func(context.Context, *sdk.Host, bool) (string, error) {
			var items []string
			for _, s := range st.list() {
				item := fmt.Sprintf("%s: %d channels, receiver %s", s.settings.Namespace, len(s.settings.Channels), s.settings.Receiver)
				if until, paused := s.client.PausedUntil(); paused {
					item += ", rate limited until " + until.UTC().Format(time.RFC3339)
				}
				items = append(items, item)
			}
			if len(items) == 0 {
				return "no Slack entries are configured", nil
			}
			return strings.Join(items, "; "), nil
		},
		"slack-auth": func(ctx context.Context, _ *sdk.Host, _ bool) (string, error) {
			var items []string
			var failed bool
			for _, s := range st.list() {
				r := sl.CheckAuth(ctx, s.client, s.settings)
				failed = failed || r.Severity == sl.DoctorFail
				items = append(items, s.settings.Namespace+": "+r.Item)
			}
			detail := strings.Join(items, "; ")
			if failed {
				return detail, errors.New("slack credential check failed")
			}
			return detail, nil
		},
	}
	ext.Webhooks = func(path string) (http.Handler, bool) {
		name := strings.TrimPrefix(path, "/")
		st.mu.Lock()
		s := st.sources[name]
		st.mu.Unlock()
		if s == nil || s.settings.Receiver != sl.ReceiverHTTP {
			return nil, false
		}
		secret := func(ctx context.Context) (string, error) {
			values, err := st.host.Load().Secrets(ctx, domain.ID(s.scope), s.settings.SigningSecret)
			if err != nil || values[s.settings.SigningSecret] == "" {
				return "", sl.ErrCredential
			}
			return values[s.settings.SigningSecret], nil
		}
		return sl.WebhookHandler(secret, st.push(s.name), o.Now), true
	}
	ext.Setups = map[string]extension.Setup{sl.Type: sl.Setup{}}
	return ext
}

// push hands a delivery to the host, which answers after durable
// acceptance; the receiver acknowledges Slack only after it returns.
func (st *state) push(name string) func(context.Context, extension.Delivery) error {
	return func(ctx context.Context, d extension.Delivery) error {
		_, err := st.host.Load().Deliver(ctx, name, protocol.FromDelivery(d))
		return err
	}
}

// activeThreads lists the threads live Work owns in an entry's namespace
// (bootstrap's in-process filter, here a scoped state.works read).
func (st *state) activeThreads(s sl.Settings) func(context.Context) (map[string][]string, error) {
	return func(ctx context.Context) (map[string][]string, error) {
		out := map[string][]string{}
		err := st.store.Transact(ctx, storage.Guard{Now: st.o.Now()}, func(tx storage.Tx) error {
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

// configure applies one generation. An unchanged entry keeps its client,
// ingress memo and running socket; a changed or removed one is replaced or
// stopped.
func (st *state) configure(ctx context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	var out protocol.ConfigureResult
	st.host.Store(h)
	next := map[string]*source{}
	st.mu.Lock()
	prev := st.sources
	st.mu.Unlock()
	var families []protocol.EventFamily
	for _, f := range sl.Families() {
		families = append(families, protocol.EventFamily{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, e := range p.Entries {
		if e.Type != sl.Type {
			continue
		}
		settings, err := sl.ParseExtension(e.Config, e.Secrets)
		if err != nil || settings.Scope != e.ScopeID {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid slack entry")
		}
		if !ValidNamespace(settings.Namespace) {
			return out, protocol.Errorf(protocol.CodeInvalid, "a packaged Slack namespace must be slack or begin with slack/")
		}
		if e.Connection == nil || e.Connection.Type != sl.Type {
			return out, protocol.Errorf(protocol.CodeInvalid, "a slack entry needs a slack connection")
		}
		entry := e.Name
		if entry == "" {
			entry = "entry"
		}
		name := Name + "/" + e.ScopeID + "/" + entry
		if p.DryRun {
			settings.Projection.DryRun = true
		}
		digest, _ := json.Marshal(struct {
			E protocol.ConfigEntry
			D bool
		}{e, p.DryRun})
		if old, ok := prev[name]; ok && old.config == string(digest) {
			next[name] = old
		} else {
			cfg, err := sl.ParseConnection(e.Connection.Config)
			if err != nil {
				return out, protocol.Errorf(protocol.CodeInvalid, "invalid slack connection")
			}
			scope, conn := domain.ID(e.ScopeID), e.Connection.ID
			secrets := func(ctx context.Context, secret string) (string, error) {
				values, err := st.host.Load().ConnectionSecrets(ctx, scope, conn, secret)
				if err != nil || values[secret] == "" {
					return "", sl.ErrCredential
				}
				return values[secret], nil
			}
			client, err := sl.NewClient(cfg, secrets, sl.ClientOptions{Now: st.o.Now, HTTP: st.o.HTTP})
			if err != nil {
				return out, protocol.Errorf(protocol.CodeInvalid, "slack client")
			}
			projector, err := sl.NewProjector(sl.ProjectorOptions{Store: st.store, Client: client, Settings: settings, Now: st.o.Now})
			if err != nil {
				return out, err
			}
			src := &source{name: name, scope: e.ScopeID, config: string(digest), settings: settings, client: client, projector: projector, stop: func() {}}
			src.ingress = sl.NewIngress(settings, client, st.o.Now, sl.IngressOptions{ActiveThreads: st.activeThreads(settings)})
			if settings.Receiver == sl.ReceiverSocket {
				sock := sl.NewSocket(client, sl.SocketOptions{Now: st.o.Now})
				sctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { defer close(done); sock.Run(sctx, st.push(name)) }()
				src.stop = func() { cancel(); <-done }
			}
			next[name] = src
		}
		out.Sources = append(out.Sources, protocol.SourceDecl{Name: name, Source: protocol.Source{Namespace: settings.Namespace, ScopeID: e.ScopeID}, Families: families})
		if settings.Receiver == sl.ReceiverHTTP {
			out.WebhookPaths = append(out.WebhookPaths, "/"+name)
		}
	}
	st.mu.Lock()
	st.sources = next
	st.mu.Unlock()
	for name, old := range prev {
		if next[name] != old {
			old.stop()
		}
	}
	return out, nil
}
