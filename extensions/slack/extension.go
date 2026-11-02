package slack

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

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/slack/playbooks"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// Slack as an in-process extension, composed by the host through the
// public contract alone (#357). Each enabled scope's `slack` entry
// registers an ingress pair (push over Socket Mode by default, or the
// Events API on the webhook listener; and reconcile), the `slack` route
// starts `slack.conversation` Work per thread, the projector posts marked
// replies from outcomes and tool intents, and the slack.* operations join
// every generation's toolbelt. extensions/slack/pkg serves the same pieces out of
// process.

type source struct {
	name      string
	scope     domain.ID
	config    json.RawMessage
	settings  Settings
	client    *Client
	ingress   *Ingress
	projector *Projector
	socket    *Socket // receiver socket only
	started   bool
	// deliver is the code of the last failed delivery to the host ("" once
	// one succeeds): an unregistered source refuses every push.
	deliver string
}

// Extension is the Slack extension.
type Extension struct {
	h     inproc.Host
	tools *Tools

	mu       sync.Mutex
	sources  map[string]*source // by namespace, for the daemon's lifetime
	failed   map[string]string
	replies  map[string]string // last projection code by namespace
	next     int               // the entry the next projection pass starts at
	codes    map[string]string
	sockets  sync.WaitGroup
	stop     context.CancelFunc
	stopped  bool
	stopping context.Context
}

// New returns the Slack extension.
func New() *Extension {
	ctx, cancel := context.WithCancel(context.Background())
	return &Extension{sources: map[string]*source{}, failed: map[string]string{}, replies: map[string]string{}, codes: map[string]string{}, stopping: ctx, stop: cancel}
}

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Operator   = (*Extension)(nil)
	_ inproc.Declarer   = (*Extension)(nil)
	_ inproc.Router     = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Stopper    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Ingester   = (*Extension)(nil)
	_ inproc.Webhooker  = (*Extension)(nil)
	_ inproc.Tooled     = (*Extension)(nil)
	_ inproc.Tasker     = (*Extension)(nil)
	_ inproc.Checker    = (*Extension)(nil)
	_ inproc.Renderer   = (*Extension)(nil)
	_ inproc.Playbooker = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateConnection, Type: Type, Validate: func(c json.RawMessage, _ []string) error { return ValidateConnection(c) }},
		{Kind: protocol.ValidateExtension, Type: Type, Ingress: true, Validate: ValidateExtension},
		{Kind: protocol.ValidateRoute, Type: Type, Validate: func(c json.RawMessage, _ []string) error { return ValidateRoute(c) }},
	}
}

func (*Extension) Operations() []string { return Operations() }

func (*Extension) Declarations() []extension.Operation { return Declared() }

func (*Extension) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{Type: Route{}}
}

func (*Extension) Playbooks() map[string]string { return playbooks.All() }

func (*Extension) Render(_ context.Context, e domain.Event) (string, bool) { return RenderEvent(e) }

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.h = h
	tools, err := NewTools(ToolsOptions{Store: h.Store, Sources: x.toolSources, Now: x.now})
	if err != nil {
		return err
	}
	x.tools = tools
	return nil
}

// Stop ends the Socket Mode receivers. Their unacknowledged envelopes are
// redelivered by Slack, and reconcile recovers the rest.
func (x *Extension) Stop() {
	// startSockets starts no receiver once stopped is set, and setting it
	// under mu waits out one in progress, so Wait sees every Add.
	x.mu.Lock()
	x.stopped = true
	x.mu.Unlock()
	x.stop()
	x.sockets.Wait()
}

func (x *Extension) Registration() extension.Registration {
	if x.tools == nil {
		return extension.Registration{}
	}
	return x.tools.Registration()
}

func (x *Extension) now() time.Time {
	if x.h.Now != nil {
		return x.h.Now()
	}
	return time.Now()
}

func (x *Extension) report(task, code string) {
	x.mu.Lock()
	x.codes[task] = code
	var codes []string
	for _, t := range []string{"register", "projection"} {
		if c := x.codes[t]; c != "" {
			codes = append(codes, c)
		}
	}
	x.mu.Unlock()
	if x.h.Report != nil {
		x.h.Report(strings.Join(codes, ","))
	}
}

// codeError is a fixed, secret-free registration diagnostic.
type codeError string

func (e codeError) Error() string { return string(e) }

// Configure registers every enabled scope's entry not yet registered.
// Registrations are immutable for one runtime instance: a changed entry
// keeps its registration until restart and reports restart_required.
func (x *Extension) Configure(_ context.Context, p protocol.ConfigureParams) {
	restart := false
	for _, e := range p.Entries {
		if e.Type != Type {
			continue
		}
		settings, err := ParseExtension(e.Config, e.Secrets)
		if err != nil || settings.Scope != e.ScopeID {
			continue
		}
		x.mu.Lock()
		prev, ok := x.sources[settings.Namespace]
		x.mu.Unlock()
		if ok {
			restart = restart || string(prev.config) != string(e.Config)
			continue
		}
		code := ""
		if err := x.add(e, settings); err != nil {
			var named codeError
			if code = ErrorCode(err); errors.As(err, &named) {
				code = string(named)
			}
		}
		x.mu.Lock()
		if code != "" {
			x.failed[settings.Namespace] = code
		} else {
			delete(x.failed, settings.Namespace)
		}
		x.mu.Unlock()
	}
	x.mu.Lock()
	var failed []string
	for _, c := range x.failed {
		failed = append(failed, c)
	}
	x.mu.Unlock()
	code := ""
	if restart {
		code = "restart_required"
	} else if len(failed) > 0 {
		sort.Strings(failed)
		code = failed[0]
	}
	x.report("register", code)
}

func (x *Extension) add(e protocol.ConfigEntry, settings Settings) error {
	if e.Connection == nil {
		return codeError("slack_connection_required")
	}
	cfg, err := ParseConnection(e.Connection.Config)
	if err != nil {
		return codeError("slack_connection_invalid")
	}
	scope, conn := domain.ID(e.ScopeID), e.Connection.ID
	secrets := func(ctx context.Context, name string) (string, error) {
		values, err := x.h.Secrets(ctx, scope, conn, []string{name})
		if err != nil {
			return "", ErrCredential
		}
		return values[name], nil
	}
	client, err := NewClient(cfg, secrets, ClientOptions{Now: x.now})
	if err != nil {
		return err
	}
	if x.h.DryRun {
		settings.Projection.DryRun = true
	}
	projector, err := NewProjector(ProjectorOptions{Store: x.h.Store, Client: client, Settings: settings, Now: x.now,
		Actions: x.h.Actions, Act: x.h.Act})
	if err != nil {
		return err
	}
	s := &source{name: Type + "/" + e.ScopeID + "/" + e.Name, scope: scope, config: append(json.RawMessage(nil), e.Config...), settings: settings, client: client, projector: projector}
	s.ingress = NewIngress(settings, client, x.now, IngressOptions{ActiveThreads: x.activeThreads(settings)})
	if settings.Receiver == ReceiverSocket {
		name := s.name
		s.socket = NewSocket(client, SocketOptions{Now: x.now, Interact: func(ctx context.Context, payload []byte) {
			if err := projector.Interact(ctx, payload); err != nil && x.h.Log != nil {
				x.h.Log.Warn("slack button press not taken", "name", name, "code", ErrorCode(err))
			}
		}})
	}
	x.mu.Lock()
	x.sources[settings.Namespace] = s
	x.mu.Unlock()
	return nil
}

// push hands a delivery to the host, which answers after durable
// acceptance; the receiver acknowledges Slack only after it returns. A
// refusal is kept for the entry's doctor line.
func (x *Extension) push(s *source) func(context.Context, extension.Delivery) error {
	return func(ctx context.Context, d extension.Delivery) error {
		err := x.h.Deliver(ctx, s.name, d)
		code := ""
		if err != nil {
			code = "deliver_failed"
		}
		x.mu.Lock()
		s.deliver = code
		x.mu.Unlock()
		return err
	}
}

// startSockets starts the Socket Mode receiver of every registered entry
// that has none running yet. It runs as a task, so after configure has
// registered the entries' sources. Receivers stop with the daemon; their
// unacknowledged envelopes are redelivered by Slack and reconcile recovers
// the rest.
func (x *Extension) startSockets(context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, s := range x.sources {
		if s.socket == nil || s.started || x.stopped {
			continue
		}
		s.started = true
		x.sockets.Add(1)
		go func(s *source) {
			defer x.sockets.Done()
			s.socket.Run(x.stopping, x.push(s))
		}(s)
	}
	return nil
}

// activeThreads lists the threads live Work owns in an entry's namespace,
// so reconcile keeps reading their replies after the parent leaves the
// history window. It only reads durable state.
func (x *Extension) activeThreads(s Settings) func(context.Context) (map[string][]string, error) {
	return func(ctx context.Context) (map[string][]string, error) {
		out := map[string][]string{}
		err := x.h.Store.Transact(ctx, storage.Guard{Now: x.now()}, func(tx storage.Tx) error {
			works, err := tx.ListWorks()
			for _, w := range works {
				if string(w.ScopeID) != s.Scope || w.State.Terminal() {
					continue
				}
				for _, k := range w.CorrelationKeys {
					if k.Source != s.Namespace {
						continue
					}
					if channel, thread, ok := ParseThreadKey(k.ExternalID); ok {
						out[channel] = append(out[channel], thread)
					}
				}
			}
			return err
		})
		return out, err
	}
}

func (x *Extension) list() []*source {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]*source, 0, len(x.sources))
	for _, s := range x.sources {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (x *Extension) toolSources() []Source {
	var out []Source
	for _, s := range x.list() {
		out = append(out, Source{Settings: s.settings, Client: s.client, Projector: s.projector})
	}
	return out
}

func (x *Extension) Sources() []inproc.Source {
	var out []inproc.Source
	for _, s := range x.list() {
		out = append(out, inproc.Source{Name: s.name, Source: extension.Source{Namespace: s.settings.Namespace, ScopeID: s.scope}, Ingress: s.ingress})
	}
	return out
}

// Webhook serves the Events API of an entry with receiver http (POST
// /slack/<scope>/<entry>).
func (x *Extension) Webhook(path string) http.Handler {
	for _, s := range x.list() {
		if s.name == Type+"/"+path && s.settings.Receiver == ReceiverHTTP {
			return WebhookHandler(x.signingSecret(s), x.push(s), x.now)
		}
	}
	return nil
}

// signingSecret resolves an entry's signing_secret among its selected
// secrets.
func (x *Extension) signingSecret(s *source) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name := s.settings.SigningSecret
		if name == "" || x.h.Secrets == nil {
			return "", ErrCredential
		}
		values, err := x.h.Secrets(ctx, s.scope, "", []string{name})
		if err != nil || values[name] == "" {
			return "", ErrCredential
		}
		return values[name], nil
	}
}

// Tasks: the Socket Mode receivers started once their sources are
// registered, and one bounded reply pass every few seconds.
func (x *Extension) Tasks() []inproc.Task {
	return []inproc.Task{
		{Name: "sockets", Pace: time.Second, Run: x.startSockets},
		{Name: "projection", Pace: 5 * time.Second, Run: x.project},
	}
}

// project converges the entries in turn, starting after the one the last
// pass reached, so a pass its deadline cuts short never starves the later
// entries; one the deadline interrupted keeps its last code.
func (x *Extension) project(ctx context.Context) error {
	sources := x.list()
	x.mu.Lock()
	start := x.next
	x.mu.Unlock()
	for i := range sources {
		k := (start + i) % len(sources)
		s := sources[k]
		if ctx.Err() != nil {
			break
		}
		res, err := s.projector.Converge(ctx, 100)
		code := strings.Join(res.Codes, ",")
		if err != nil && code == "" {
			code = ErrorCode(err)
		}
		x.mu.Lock()
		if ctx.Err() == nil {
			x.replies[s.settings.Namespace] = code
		}
		x.next = k + 1
		x.mu.Unlock()
	}
	var codes []string
	x.mu.Lock()
	for _, s := range sources {
		if c := x.replies[s.settings.Namespace]; c != "" {
			codes = append(codes, c)
		}
	}
	x.mu.Unlock()
	x.report("projection", strings.Join(codes, ","))
	return nil
}

func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Run: x.check}, {Deep: true, Run: x.authCheck}}
}

var rank = map[string]int{inproc.CheckOK: 0, inproc.CheckInfo: 1, inproc.CheckWarn: 2, inproc.CheckFail: 3}

// check reports registration, rate-limit, reply, receiver and socket state
// per entry. Reconcile health is the host's reconcile_lag check. It makes
// no service calls; slack_auth tests credentials.
func (x *Extension) check(context.Context, protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "slack", Status: inproc.CheckOK, Code: "ok"}
	raise := func(status, code string) {
		if rank[status] > rank[check.Status] {
			check.Status, check.Code = status, code
		}
	}
	x.mu.Lock()
	failed := map[string]string{}
	for ns, c := range x.failed {
		failed[ns] = c
	}
	replies := map[string]string{}
	for ns, c := range x.replies {
		replies[ns] = c
	}
	delivers := map[string]string{}
	for ns, s := range x.sources {
		delivers[ns] = s.deliver
	}
	x.mu.Unlock()
	sources := x.list()
	if len(sources) == 0 && len(failed) == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", "no Slack entries are configured"
		return check
	}
	for ns, code := range failed {
		raise(inproc.CheckFail, code)
		check.Items = append(check.Items, ns+": not registered ("+code+")")
	}
	for _, s := range sources {
		set := s.settings
		var notes []string
		if until, paused := s.client.PausedUntil(); paused {
			raise(inproc.CheckWarn, "rate_limited")
			notes = append(notes, "rate limited until "+until.UTC().Format(time.RFC3339))
		}
		if code := delivers[set.Namespace]; code != "" {
			raise(inproc.CheckFail, code)
			notes = append(notes, "deliveries refused by the host (is the source registered? see extension_ingress)")
		}
		if code := replies[set.Namespace]; code != "" {
			raise(inproc.CheckWarn, "replies_failing")
			notes = append(notes, "replies "+code)
		}
		notes = append(notes, fmt.Sprintf("%d channels, start %s, receiver %s", len(set.Channels), set.Start, set.Receiver))
		switch set.Receiver {
		case ReceiverHTTP:
			notes = append(notes, "POST /"+s.name+" (set it as the app's Events API Request URL)")
		case ReceiverSocket:
			st := s.socket.Status()
			switch {
			case st.Connected:
				notes = append(notes, fmt.Sprintf("socket connected (%d acknowledged)", st.Acked))
			case st.LastError != "":
				raise(inproc.CheckWarn, "socket_"+st.LastError)
				notes = append(notes, "socket down ("+st.LastError+"), retrying at "+st.RetryUntil.UTC().Format(time.RFC3339))
			default:
				notes = append(notes, "socket connecting")
			}
			if st.Unacked > 0 {
				notes = append(notes, fmt.Sprintf("%d envelopes left unacknowledged for redelivery", st.Unacked))
			}
		default:
			notes = append(notes, "reconcile only")
		}
		if x.h.DryRun || set.Projection.DryRun {
			notes = append(notes, "dry run (no Slack writes)")
		}
		check.Items = append(check.Items, set.Namespace+": "+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
}

// authCheck (check_secrets only) runs the connection test per entry
// (CheckAuth: team, bot user and the granted scopes compared with the
// manifest). It reports codes, never the reference or the token.
func (x *Extension) authCheck(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "slack_auth", Status: inproc.CheckOK, Code: "verified"}
	sources := x.list()
	if len(sources) == 0 {
		check.Status, check.Code = inproc.CheckInfo, "not_configured"
		return check
	}
	for _, s := range sources {
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		r := CheckAuth(cx, s.client, s.settings)
		cancel()
		status := inproc.CheckOK
		switch r.Severity {
		case DoctorFail:
			status = inproc.CheckFail
		case DoctorWarn:
			status = inproc.CheckWarn
		}
		if rank[status] > rank[check.Status] {
			check.Status, check.Code = status, r.Code
		}
		check.Items = append(check.Items, s.settings.Namespace+": "+r.Item)
	}
	sort.Strings(check.Items)
	return check
}
