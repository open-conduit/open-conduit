package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/github/playbooks"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// The GitHub extension's own entries (#357): each enabled scope's `github`
// entry registers an ingress pair (webhook push on the listener when it
// names a webhook_secret or its connection does, and reconcile), its projector converges tracker
// labels and marked comments, a manage_webhook entry has its repository
// webhook pointed at the public URL, and the github.* tools join every
// generation's toolbelt, all through the public in-process contract.

const (
	passLimit       = 100
	projectionPace  = 5 * time.Second
	hookRetry       = time.Minute
	gitTokenPace    = 5 * time.Second
	hookCallTimeout = 30 * time.Second
	// hookCheckEvery is how often a managed hook's last delivery is read
	// back from GitHub, and hookPingWait how long after a re-point (and its
	// ping) the first read comes.
	hookCheckEvery = 5 * time.Minute
	hookPingWait   = 30 * time.Second
)

// entry is one registered `github` entry.
type entry struct {
	name       string
	scope      domain.ID
	config     json.RawMessage
	connection string
	conn       string // the connection config and secret names the client was set up from
	client     *Client
	ingress    *Ingress
	projector  *Projector
	projection string // the last projection pass's code
	// shared is whether the entry's connection names a webhook_secret, so
	// the connection's webhook route delivers its repository's deliveries.
	shared bool
}

// unmatched records the deliveries a webhook route verified that no entry
// was for, for the github check.
type unmatched struct {
	count             int
	at                time.Time
	event, repository string
}

// sharedMemo is the read memo of a delivery being delivered to several
// entries, with how many deliveries of it are in progress.
type sharedMemo struct {
	m    *memo
	refs int
}

type hook struct {
	url, id, code string
	next          time.Time
	// checked is when the hook's last delivery was last read back from
	// GitHub; delivery says what GitHub recorded for it, and failing
	// whether that was a failure.
	checked  time.Time
	delivery string
	failing  bool
}

// registry is the extension's entries for the daemon's lifetime: a
// registration is immutable, so a changed entry reports restart_required.
type registry struct {
	mu        sync.Mutex
	entries   map[string]*entry // by namespace
	failed    map[string]string // registration failure code by namespace
	hooks     map[string]*hook  // by namespace
	public    string
	codes     map[string]string
	next      int  // the entry the next projection pass starts at
	projected bool // the last projection pass wrote something
	// unmatched is by route ("github/<connection>" or an entry's name).
	unmatched map[string]*unmatched
	// memos are the deliveries a connection's webhook route is delivering,
	// by delivery ID.
	memos map[string]*sharedMemo
	// budgets are the rate-limit budgets every client here shares.
	budgets *Budgets
	// converge stands in for an entry's projector in tests.
	converge func(ctx context.Context, e *entry, limit int) (ProjectionResult, error)
}

// New returns the GitHub extension the daemon composes: it registers its
// own entries.
func New() *Extension {
	x := NewExtension(nil)
	x.own = &registry{entries: map[string]*entry{}, failed: map[string]string{}, hooks: map[string]*hook{}, codes: map[string]string{},
		unmatched: map[string]*unmatched{}, memos: map[string]*sharedMemo{}, budgets: NewBudgets()}
	return x
}

var (
	_ inproc.Operator      = (*Extension)(nil)
	_ inproc.Declarer      = (*Extension)(nil)
	_ inproc.Router        = (*Extension)(nil)
	_ inproc.Ingester      = (*Extension)(nil)
	_ inproc.Webhooker     = (*Extension)(nil)
	_ inproc.WebhookPather = (*Extension)(nil)
	_ inproc.Tooled        = (*Extension)(nil)
	_ inproc.Tasker        = (*Extension)(nil)
	_ inproc.Actor         = (*Extension)(nil)
	_ inproc.Annotator     = (*Extension)(nil)
	_ inproc.Renderer      = (*Extension)(nil)
	_ inproc.SegmentEnder  = (*Extension)(nil)

	_ inproc.Executing = (*Extension)(nil)

	_ extension.RunEnvironment = (*Extension)(nil)
	_ extension.SetupProbes    = (*Extension)(nil)
	_ inproc.Playbooker        = (*Extension)(nil)
)

// Executors is the hold executor that runs held issues, a job.
func (*Extension) Executors() map[string]inproc.Executor {
	return map[string]inproc.Executor{HoldExecutor: {Executor: NewHoldExecutor(), Pool: inproc.PoolJobs}}
}

// SetupProbes are the GitHub probes a setup session may run.
func (*Extension) SetupProbes() []extension.SetupProbe { return Probes{}.SetupProbes() }

// Playbooks are the GitHub playbooks (work-issue), which the host writes
// into the plugin agents load.
func (*Extension) Playbooks() map[string]string { return playbooks.All() }

func (*Extension) Operations() []string { return Operations() }

func (*Extension) Declarations() []extension.Operation { return Declared() }

func (*Extension) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{Type: Route{}, HoldHandler: HoldRoute{}}
}

func (*Extension) Render(_ context.Context, e domain.Event) (string, bool) { return RenderEvent(e) }

// startTools builds the tools once the host services exist.
func (x *Extension) startTools(h inproc.Host) error {
	if x.own == nil || h.Store == nil {
		return nil
	}
	var jobs func(context.Context, domain.ID, string) (domain.CorrelationKey, []string, bool)
	if h.WaitTarget != nil {
		jobs = func(ctx context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
			return h.WaitTarget(ctx, scope, "job", name)
		}
	}
	authorize := func(kind string) func(from, to domain.ID) error {
		if h.Authorize == nil {
			return nil
		}
		return func(from, to domain.ID) error { return h.Authorize(kind, from, to) }
	}
	// A PR Work without an issue opened is given to it as a TypePullOwned
	// Event in its attempt's causal chain.
	own := func(ctx context.Context, scope, cause domain.ID, e domain.Event) error {
		return h.Store.Transact(ctx, storage.Guard{Now: x.now()}, func(tx storage.Tx) error {
			var err error
			switch {
			case cause != "" && h.AcceptDerivedInTx != nil:
				_, _, err = h.AcceptDerivedInTx(tx, scope, cause, e)
			case h.AcceptInTx != nil:
				_, _, err = h.AcceptInTx(tx, scope, e)
			default:
				err = storage.ErrInvalid
			}
			return err
		})
	}
	tools, err := NewTools(ToolsOptions{Store: h.Store, Sources: x.toolSources, Now: x.now, Jobs: jobs, WaitProgress: h.WaitProgress, Pending: h.Pending, Writers: x.clients, MayRead: authorize("read"), MayFile: authorize("file"), Own: own,
		WakeSweep: func() { x.wakeTask("git_credentials") }})
	if err != nil {
		return err
	}
	x.mu.Lock()
	x.tools = tools
	x.mu.Unlock()
	return nil
}

func (x *Extension) svc() inproc.Host {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.host
}

func (x *Extension) Registration() extension.Registration {
	x.mu.Lock()
	t := x.tools
	x.mu.Unlock()
	if t == nil {
		return extension.Registration{}
	}
	return t.Registration()
}

func (x *Extension) report(task, code string) {
	r := x.own
	r.mu.Lock()
	r.codes[task] = code
	var codes []string
	for _, t := range []string{"register", "projection", "webhook", "git_credentials"} {
		if c := r.codes[t]; c != "" {
			codes = append(codes, c)
		}
	}
	r.mu.Unlock()
	if h := x.svc(); h.Report != nil {
		h.Report(strings.Join(codes, ","))
	}
}

// register registers every enabled scope's entry not yet registered, and
// points a registered entry's client at its connection when that changed.
func (x *Extension) register(p protocol.ConfigureParams) {
	r := x.own
	restart := false
	for _, e := range p.Entries {
		if e.Type != Type {
			continue
		}
		settings, err := ParseExtension(e.Config, e.Secrets)
		if err != nil || settings.Scope != e.ScopeID {
			continue
		}
		conn := ""
		if e.Connection != nil {
			conn = e.Connection.ID
		}
		r.mu.Lock()
		prev, ok := r.entries[settings.Namespace]
		r.mu.Unlock()
		if ok {
			if string(prev.config) != string(e.Config) || prev.connection != conn {
				restart = true
			} else if !x.follow(prev, e.Connection) {
				restart = true
			}
			continue
		}
		code := ""
		if err := x.add(e, settings); err != nil {
			var named codeError
			if code = CallCode(err); errors.As(err, &named) {
				code = string(named)
			}
		}
		r.mu.Lock()
		if code != "" {
			r.failed[settings.Namespace] = code
		} else {
			delete(r.failed, settings.Namespace)
		}
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.public = p.PublicURL
	var failed []string
	for _, c := range r.failed {
		failed = append(failed, c)
	}
	r.mu.Unlock()
	code := ""
	if restart {
		code = "restart_required"
	} else if len(failed) > 0 {
		sort.Strings(failed)
		code = failed[0]
	}
	x.report("register", code)
}

// codeError is a fixed, secret-free registration diagnostic.
type codeError string

func (e codeError) Error() string { return string(e) }

// connectionKey identifies what a connection's client is built from: its
// config and secret names.
func connectionKey(c *protocol.ConnectionEntry) string {
	names, _ := json.Marshal(c.Secrets)
	return string(c.Config) + "\x00" + string(names)
}

func (x *Extension) secrets(scope domain.ID, conn string) SecretFunc {
	return func(ctx context.Context, name string) (string, error) {
		h := x.svc()
		if h.Secrets == nil {
			return "", ErrCredential
		}
		v, err := h.Secrets(ctx, scope, conn, []string{name})
		if err != nil || v[name] == "" {
			return "", ErrCredential
		}
		return v[name], nil
	}
}

func (x *Extension) add(e protocol.ConfigEntry, settings Settings) error {
	if e.Connection == nil {
		return codeError("github_connection_required")
	}
	cfg, err := ParseConnection(e.Connection.Config)
	if err != nil {
		return codeError("github_connection_invalid")
	}
	scope := domain.ID(e.ScopeID)
	client, err := NewClient(cfg, x.secrets(scope, e.Connection.ID), ClientOptions{Now: x.now, Budgets: x.own.budgets})
	if err != nil {
		return err
	}
	h := x.svc()
	if h.DryRun {
		// A daemon dry run makes no GitHub writes: projections and tool
		// writes report dry_run.
		settings.Projection.DryRun = true
	}
	in := NewIngress(settings, client, x.now, IngressOptions{Observed: func(ctx context.Context, obs []extension.Observation) {
		x.mu.Lock()
		t := x.tools
		x.mu.Unlock()
		if t != nil {
			t.ObserveChecks(ctx, obs)
		}
	}, Memo: x.sharedMemo})
	projector, err := NewProjector(ProjectorOptions{Store: h.Store, Client: client, Settings: settings, Now: x.now, Writers: x.clients, Scope: scope, Act: h.Act,
		Wake: func() { x.wakeTask("projection") }})
	if err != nil {
		return err
	}
	x.own.mu.Lock()
	x.own.entries[settings.Namespace] = &entry{name: Type + "/" + e.ScopeID + "/" + e.Name, scope: scope, config: append(json.RawMessage(nil), e.Config...),
		connection: e.Connection.ID, conn: connectionKey(e.Connection), client: client, ingress: in, projector: projector,
		shared: slices.Contains(e.Connection.Secrets, SecretWebhook)}
	x.own.mu.Unlock()
	return nil
}

// follow points a registered entry's client at its connection's current
// config when it changed, so an applied bot App or credential change takes
// effect without a restart. A change it can't apply in place (another
// api_url) reports false.
func (x *Extension) follow(e *entry, c *protocol.ConnectionEntry) bool {
	if c == nil {
		return true // config validation refuses an entry without its connection
	}
	key := connectionKey(c)
	x.own.mu.Lock()
	same := key == e.conn
	x.own.mu.Unlock()
	if same {
		return true
	}
	cfg, err := ParseConnection(c.Config)
	if err == nil {
		err = e.client.Reconfigure(cfg, x.secrets(e.scope, c.ID))
	}
	if err != nil {
		return false
	}
	x.own.mu.Lock()
	e.conn, e.shared = key, slices.Contains(c.Secrets, SecretWebhook)
	x.own.mu.Unlock()
	if h := x.svc(); h.Log != nil {
		h.Log.Info("github connection change applied", "name", e.name, "connection", c.ID)
	}
	return true
}

func (x *Extension) list() []*entry {
	if x.own == nil {
		return nil
	}
	x.own.mu.Lock()
	defer x.own.mu.Unlock()
	out := make([]*entry, 0, len(x.own.entries))
	for _, e := range x.own.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (x *Extension) toolSources() []Source {
	var out []Source
	for _, e := range x.list() {
		out = append(out, Source{Settings: e.ingress.Settings(), Connection: e.connection, Client: e.client, Projector: e.projector})
	}
	return out
}

func (x *Extension) Sources() []inproc.Source {
	var out []inproc.Source
	for _, e := range x.list() {
		out = append(out, inproc.Source{Name: e.name, Source: extension.Source{Namespace: e.ingress.Settings().Namespace, ScopeID: e.scope}, Ingress: e.ingress, Interval: e.ingress.Settings().Reconcile})
	}
	return out
}

// Webhook serves an entry's webhook (POST /github/<scope>/<entry>) when it
// names a webhook_secret, and a connection's (POST /github/<connection>)
// when the connection names one: the one URL of a GitHub App's or an
// organisation's webhook, which delivers each delivery to every entry of
// the connection whose repository it is about.
func (x *Extension) Webhook(path string) http.Handler {
	if !strings.Contains(path, "/") {
		return x.connectionWebhook(path)
	}
	for _, e := range x.list() {
		if e.name == Type+"/"+path && e.ingress.Settings().WebhookSecret != "" {
			src := e
			route := func(ctx context.Context, d extension.Delivery) (Routed, error) {
				event, repo, err := Target(d)
				if err != nil {
					return Routed{}, err
				}
				if !src.ingress.For(repo) && (event != "ping" || repo != "") {
					x.recordUnmatched(src.name, event, repo)
					return Routed{}, nil
				}
				return x.deliver(ctx, []*entry{src}, d)
			}
			return RoutedHandler(x.webhookSecret(src), route, x.now)
		}
	}
	return nil
}

// WebhookPaths are the connections' webhook routes: each connection of an
// entry that names a webhook_secret.
func (x *Extension) WebhookPaths() []string {
	var out []string
	for _, e := range x.list() {
		x.own.mu.Lock()
		shared := e.shared
		x.own.mu.Unlock()
		if shared && !slices.Contains(out, e.connection) {
			out = append(out, e.connection)
		}
	}
	return out
}

// connectionWebhook serves the webhook of connection conn, verified with
// its webhook_secret, or nil when none of its entries' connection names one.
func (x *Extension) connectionWebhook(conn string) http.Handler {
	var entries []*entry
	for _, e := range x.list() {
		if x.own == nil || e.connection != conn {
			continue
		}
		x.own.mu.Lock()
		shared := e.shared
		x.own.mu.Unlock()
		if shared {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil
	}
	// Any of the entries resolves the connection's secret: each is
	// authorized for its own connection.
	first := entries[0]
	secret := func(ctx context.Context) (string, error) { return x.secrets(first.scope, conn)(ctx, SecretWebhook) }
	route := func(ctx context.Context, d extension.Delivery) (Routed, error) {
		event, repo, err := Target(d)
		if err != nil {
			return Routed{}, err
		}
		var to []*entry
		for _, e := range entries {
			if repo != "" && e.ingress.For(repo) {
				to = append(to, e)
			}
		}
		if len(to) == 0 {
			if event == "ping" && repo == "" {
				return Routed{Acknowledged: true}, nil
			}
			x.recordUnmatched(Type+"/"+conn, event, repo)
			return Routed{}, nil
		}
		return x.deliver(ctx, to, d)
	}
	return RoutedHandler(secret, route, x.now)
}

// deliver hands a verified delivery to each entry in turn, sharing one
// read memo among them. Every entry is tried even after one fails, so a
// redelivery finds the rest already accepted; any failure fails the whole.
func (x *Extension) deliver(ctx context.Context, to []*entry, d extension.Delivery) (Routed, error) {
	if x.own != nil && len(to) > 1 {
		x.own.mu.Lock()
		sm := x.own.memos[d.ID]
		if sm == nil {
			sm = &sharedMemo{m: newMemo()}
			x.own.memos[d.ID] = sm
		}
		sm.refs++
		x.own.mu.Unlock()
		defer func() {
			x.own.mu.Lock()
			if sm.refs--; sm.refs == 0 {
				delete(x.own.memos, d.ID)
			}
			x.own.mu.Unlock()
		}()
	}
	h := x.svc()
	out := Routed{Entries: len(to), Duplicate: true}
	var failed error
	for _, e := range to {
		if h.DeliverAdded == nil {
			out.Duplicate = false
			if err := h.Deliver(ctx, e.name, d); err != nil {
				failed = err
			}
			continue
		}
		accepted, added, err := h.DeliverAdded(ctx, e.name, d)
		if err != nil {
			failed = err
			if h.Log != nil {
				h.Log.Warn("github delivery not accepted", "name", e.name, "delivery", d.ID)
			}
			continue
		}
		if len(accepted) == 0 || slices.Contains(added, true) {
			out.Duplicate = false
		}
	}
	if failed != nil {
		return Routed{}, failed
	}
	return out, nil
}

// sharedMemo is the read memo of a delivery the connection's webhook route
// is delivering to several entries (IngressOptions.Memo), or nil.
func (x *Extension) sharedMemo(delivery string) *memo {
	if x.own == nil {
		return nil
	}
	x.own.mu.Lock()
	defer x.own.mu.Unlock()
	if sm := x.own.memos[delivery]; sm != nil {
		return sm.m
	}
	return nil
}

// recordUnmatched records a verified delivery that no entry of route was for:
// one about a repository none is configured for, or an App-level event
// (installation) the entries don't consume.
func (x *Extension) recordUnmatched(route, event, repo string) {
	if h := x.svc(); h.Log != nil {
		h.Log.Warn("github delivery for no configured entry", "route", route, "event", event, "repository", repo)
	}
	if x.own == nil {
		return
	}
	x.own.mu.Lock()
	defer x.own.mu.Unlock()
	u := x.own.unmatched[route]
	if u == nil {
		u = &unmatched{}
		x.own.unmatched[route] = u
	}
	u.count++
	u.at, u.event, u.repository = x.now(), event, repo
}

// webhookSecret resolves an entry's webhook_secret among its selected
// secrets.
func (x *Extension) webhookSecret(e *entry) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name := e.ingress.Settings().WebhookSecret
		if name == "" {
			return "", ErrCredential
		}
		return x.secrets(e.scope, "")(ctx, name)
	}
}

// Tasks: one bounded projection pass per entry every few seconds, the
// managed webhooks pointed at the public URL, and the git tokens of ended
// executions revoked.
func (x *Extension) Tasks() []inproc.Task {
	if x.own == nil {
		return nil
	}
	return []inproc.Task{
		{Name: "projection", Pace: projectionPace, Plan: x.planProjection},
		{Name: "webhooks", Pace: time.Second, Plan: x.planHooks},
		{Name: "git_credentials", Pace: gitTokenPace, Plan: x.planGitTokens},
	}
}

// wakeTask asks the host to run one of the tasks soon.
func (x *Extension) wakeTask(task string) {
	x.mu.Lock()
	wake := x.host.WakeTask
	x.mu.Unlock()
	if wake != nil {
		wake(task)
	}
}

// planProjection sleeps until Work changes or a comment arrives once every
// entry is quiet. A pass that did anything, or left a code, keeps the pace.
func (x *Extension) planProjection(ctx context.Context) (inproc.TaskPlan, error) {
	if err := x.project(ctx); err != nil {
		return inproc.TaskPlan{}, err
	}
	entries := x.list()
	x.own.mu.Lock()
	busy := x.own.projected
	x.own.mu.Unlock()
	if busy {
		return inproc.TaskPlan{}, nil
	}
	for _, e := range entries {
		x.own.mu.Lock()
		code := e.projection
		x.own.mu.Unlock()
		if code != "" {
			return inproc.TaskPlan{}, nil
		}
		if quiet, err := e.projector.Quiet(ctx); err != nil || !quiet {
			return inproc.TaskPlan{}, nil
		}
	}
	return inproc.TaskPlan{Families: []string{TypeCommentHuman}, Work: true}, nil
}

// planHooks sleeps until a managed hook's retry or delivery read is due. A
// new public URL reaches the task through configure.
func (x *Extension) planHooks(ctx context.Context) (inproc.TaskPlan, error) {
	if err := x.manageHooks(ctx); err != nil {
		return inproc.TaskPlan{}, err
	}
	r := x.own
	now := x.now()
	next := now.Add(protocol.MaxTaskSleep)
	r.mu.Lock()
	public := r.public
	r.mu.Unlock()
	if public == "" {
		return inproc.TaskPlan{Next: next}, nil
	}
	for _, e := range x.list() {
		s := e.ingress.Settings()
		r.mu.Lock()
		shared, st := e.shared, r.hooks[s.Namespace]
		var due time.Time
		if st != nil {
			switch {
			case st.code != "ok" && st.code != "dry_run":
				due = st.next
			case st.code == "ok" && st.id != "":
				due = st.checked.Add(hookCheckEvery)
			}
		}
		r.mu.Unlock()
		if !s.ManageWebhook || s.WebhookSecret == "" || shared {
			continue
		}
		if st == nil {
			return inproc.TaskPlan{}, nil
		}
		if !due.IsZero() && due.Before(next) {
			next = due
		}
	}
	return inproc.TaskPlan{Next: next}, nil
}

// planGitTokens sleeps while no token is out; minting one wakes it.
func (x *Extension) planGitTokens(ctx context.Context) (inproc.TaskPlan, error) {
	if err := x.sweepGitTokens(ctx); err != nil {
		return inproc.TaskPlan{}, err
	}
	x.mu.Lock()
	t := x.tools
	x.mu.Unlock()
	if t != nil && t.HasGitTokens() {
		return inproc.TaskPlan{}, nil
	}
	return inproc.TaskPlan{Next: x.now().Add(protocol.MaxTaskSleep)}, nil
}

// project converges the entries in turn, starting after the one the last
// pass reached, so a pass its deadline cuts short never starves the later
// entries; one the deadline interrupted keeps its last code.
func (x *Extension) project(ctx context.Context) error {
	entries := x.list()
	x.own.mu.Lock()
	start := x.own.next
	x.own.projected = false
	x.own.mu.Unlock()
	for i := range entries {
		k := (start + i) % len(entries)
		e := entries[k]
		if ctx.Err() != nil {
			break
		}
		converge := e.projector.Converge
		if x.own.converge != nil {
			converge = func(ctx context.Context, limit int) (ProjectionResult, error) { return x.own.converge(ctx, e, limit) }
		}
		res, err := converge(ctx, passLimit)
		code := strings.Join(res.Codes, ",")
		if err != nil && code == "" {
			code = CallCode(err)
		}
		x.own.mu.Lock()
		if ctx.Err() == nil {
			e.projection = code
		}
		x.own.projected = x.own.projected || res.Labeled+res.Posted+res.Recovered+res.Reacted > 0
		x.own.next = k + 1
		x.own.mu.Unlock()
	}
	var codes []string
	x.own.mu.Lock()
	for _, e := range entries {
		if e.projection != "" {
			codes = append(codes, e.projection)
		}
	}
	x.own.mu.Unlock()
	x.report("projection", strings.Join(codes, ","))
	return nil
}

// manageHooks points each manage_webhook entry's repository webhook at the
// public URL. A dry run (daemon or entry) only reports what it would do.
func (x *Extension) manageHooks(ctx context.Context) error {
	r := x.own
	r.mu.Lock()
	public := r.public
	r.mu.Unlock()
	if public == "" {
		return nil
	}
	code := ""
	for _, e := range x.list() {
		s := e.ingress.Settings()
		r.mu.Lock()
		shared := e.shared
		r.mu.Unlock()
		if !s.ManageWebhook || s.WebhookSecret == "" || shared {
			continue
		}
		target := public + "/" + e.name
		r.mu.Lock()
		st := r.hooks[s.Namespace]
		due := st == nil || !(st.url == target && (st.code == "ok" || st.code == "dry_run") || x.now().Before(st.next))
		existing := ""
		if st != nil {
			existing = st.id
		}
		check := !due && st.code == "ok" && st.id != "" && !x.now().Before(st.checked.Add(hookCheckEvery))
		r.mu.Unlock()
		if check {
			x.readDelivery(ctx, e, s.Namespace, st.id)
		}
		if !due {
			continue
		}
		next := &hook{url: target, id: existing, code: "ok"}
		cx, cancel := context.WithTimeout(ctx, hookCallTimeout)
		secret, err := x.webhookSecret(e)(cx)
		if err == nil {
			dry := x.svc().DryRun || s.Projection.DryRun
			var wrote bool
			next.id, wrote, err = e.client.EnsureWebhook(cx, s.Repo, target, "/"+e.name, secret, existing, dry)
			if dry {
				next.code = "dry_run"
			}
			if err == nil && wrote {
				// GitHub keeps the last delivery's result across a
				// re-point; a ping replaces it with the new URL's, so the
				// first read comes soon after one. Without the ping, the
				// read waits a full interval, for a real delivery to
				// replace the old URL's result.
				next.checked = x.now()
				if perr := e.client.PingWebhook(cx, s.Repo, next.id); perr != nil {
					if h := x.svc(); h.Log != nil {
						h.Log.Warn("github hook ping failed; its last delivery is read again later", "name", e.name, "code", CallCode(perr))
					}
				} else {
					next.checked = x.now().Add(hookPingWait - hookCheckEvery)
				}
			}
		}
		cancel()
		if err != nil {
			next.code, next.next = CallCode(err), x.now().Add(hookRetry)
		}
		r.mu.Lock()
		r.hooks[s.Namespace] = next
		r.mu.Unlock()
	}
	entries := x.list()
	r.mu.Lock()
	for _, e := range entries {
		st := r.hooks[e.ingress.Settings().Namespace]
		switch {
		case st == nil:
		case st.code != "ok" && st.code != "dry_run":
			code = st.code
		case st.failing && code == "":
			code = "hook_delivery_failing"
		}
	}
	r.mu.Unlock()
	x.report("webhook", code)
	return nil
}

// readDelivery reads back from GitHub how the hook's last delivery went, so
// doctor shows whether deliveries arrive and not only that the hook is set.
// A failed read keeps the last answer and waits for the next interval.
func (x *Extension) readDelivery(ctx context.Context, e *entry, ns, id string) {
	cx, cancel := context.WithTimeout(ctx, hookCallTimeout)
	d, err := e.client.HookDelivery(cx, e.ingress.Settings().Repo, id)
	cancel()
	r := x.own
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.hooks[ns]
	if st == nil || st.id != id {
		return
	}
	st.checked = x.now()
	if err == nil {
		st.delivery, st.failing = d.Describe(), d.Failing()
	}
}

// sweepGitTokens revokes the git tokens of every execution that isn't
// running: those whose segment ended without SegmentEnded (a stop, a lost
// lease) or whose revocation failed.
func (x *Extension) sweepGitTokens(ctx context.Context) error {
	x.mu.Lock()
	t := x.tools
	x.mu.Unlock()
	if t == nil {
		return nil
	}
	store := x.svc().Store
	err := t.SweepGitTokens(ctx, func(ids []domain.ID) (map[domain.ID]bool, error) {
		var live map[domain.ID]bool
		err := store.Transact(ctx, storage.Guard{Now: x.now()}, func(tx storage.Tx) error {
			live = map[domain.ID]bool{}
			for _, id := range ids {
				e, err := tx.Execution(id)
				if errors.Is(err, storage.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				live[id] = e.State == domain.ExecutionRunning
			}
			return nil
		})
		return live, err
	})
	code := ""
	if err != nil && ctx.Err() == nil {
		code = "revoke_failed"
	}
	x.report("git_credentials", code)
	return nil
}

// SegmentEnded revokes the git tokens the tools minted for an execution
// (github.git.credential, for the toolbelt's git transport) once its segment has
// ended. One GitHub couldn't be told about is left for the sweep.
func (x *Extension) SegmentEnded(ctx context.Context, execution domain.ID) {
	x.mu.Lock()
	t := x.tools
	x.mu.Unlock()
	if t == nil {
		return
	}
	if err := t.RevokeGitTokens(ctx, execution); err != nil {
		x.report("git_credentials", "revoke_failed")
	}
}

// RunEnvironment is the git environment of a run whose Work a GitHub entry
// owns (the tools' git transport or credential helper, and bot identity).
func (x *Extension) RunEnvironment(ctx context.Context, r extension.RunEnvironmentRequest) (map[string]string, error) {
	x.mu.Lock()
	t := x.tools
	x.mu.Unlock()
	if t == nil {
		return nil, nil
	}
	return t.RunEnvironment(ctx, r)
}

// Actions are each of a scope's entries' actions, by entry name.
func (x *Extension) Actions(scope domain.ID) map[string]extension.ActionProvider {
	out := map[string]extension.ActionProvider{}
	for _, e := range x.list() {
		if e.scope == scope {
			out[e.name] = Actions{Store: x.svc().Store, Settings: e.ingress.Settings()}
		}
	}
	return out
}

// Annotate contributes the entries' context for Work they own.
func (x *Extension) Annotate(_ context.Context, w domain.Work) []inproc.Annotation {
	var out []inproc.Annotation
	for _, e := range x.list() {
		lines, ok := Annotate(e.ingress.Settings(), e.client.Web(), w)
		if !ok {
			continue
		}
		for _, l := range lines {
			out = append(out, inproc.Annotation{Source: e.name, Name: l.Name, Value: l.Value})
		}
	}
	return out
}

// ownView is the entries as the doctor checks read them. Reconcile health
// is the host's reconcile_lag check.
func (x *Extension) ownView() View {
	r := x.own
	v := View{Failed: map[string]string{}}
	r.mu.Lock()
	for ns, code := range r.failed {
		v.Failed[ns] = code
	}
	r.mu.Unlock()
	for _, e := range x.list() {
		r.mu.Lock()
		ve := ViewEntry{Name: e.name, Scope: e.scope, Connection: e.connection, Client: e.client, Settings: e.ingress.Settings(), Projection: e.projection}
		r.mu.Unlock()
		if until, paused := e.client.PausedUntil(); paused {
			ve.PausedUntil = until
		}
		v.Entries = append(v.Entries, ve)
	}
	return v
}

// hookItems are the entries' webhook routes and hooks for the github
// check, warning on a failed hook update.
func (x *Extension) hookItems(raise func(status, code string)) map[string]string {
	out := map[string]string{}
	r := x.own
	for _, e := range x.list() {
		s := e.ingress.Settings()
		r.mu.Lock()
		shared := e.shared
		r.mu.Unlock()
		var items []string
		if shared {
			items = append(items, "POST /"+Type+"/"+e.connection+" (the connection's webhook)")
			if s.ManageWebhook {
				raise(inproc.CheckWarn, "manage_webhook_ignored")
				items = append(items, "manage_webhook ignored: the connection's webhook delivers this repository's deliveries; turn it off")
			}
		}
		if s.WebhookSecret == "" {
			if !shared {
				items = append(items, "no webhook_secret (not mounted; reconcile only)")
			}
			out[s.Namespace] = strings.Join(items, "; ")
			continue
		}
		item := "POST /" + e.name
		r.mu.Lock()
		st := r.hooks[s.Namespace]
		r.mu.Unlock()
		switch {
		case st != nil:
			item += "; hook " + st.code
			if st.code != "ok" && st.code != "dry_run" {
				raise(inproc.CheckWarn, st.code)
			} else if st.delivery != "" {
				item += "; " + st.delivery
				if st.failing {
					item += " (hook_delivery_failing: GitHub's deliveries are not arriving)"
					raise(inproc.CheckWarn, "hook_delivery_failing")
				}
			}
		case s.ManageWebhook && !shared:
			item += "; hook pending"
		}
		out[s.Namespace] = strings.Join(append(items, item), "; ")
	}
	return out
}

// unmatchedItems are the webhook routes' deliveries that no entry was for.
func (x *Extension) unmatchedItems() []string {
	if x.own == nil {
		return nil
	}
	x.own.mu.Lock()
	defer x.own.mu.Unlock()
	var out []string
	for route, u := range x.own.unmatched {
		what := u.event
		if u.repository != "" {
			what += " for " + u.repository
		}
		out = append(out, fmt.Sprintf("POST /%s: %d deliveries for no configured entry (answered 421), the last %s at %s", route, u.count, what, u.at.UTC().Format(time.RFC3339)))
	}
	return out
}
