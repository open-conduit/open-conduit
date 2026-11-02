package schedule

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
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// Scheduled jobs (05-01) as an in-process extension, composed by the host
// through the public contract alone. Each enabled scope's `schedule` entry
// registers a one-shot webhook ingress (custom POSTs at /schedule/<scope>/
// <entry>/<job>/<token> on the webhook listener); its timed jobs get durable
// core timers whose calendar recurrence and six-hour catch-up are
// Recurrence, reconciled with every configure; code-host deliveries a job's
// `on.events` selects become derived job triggers; and the `schedule` route
// makes every trigger input of the job's standing Work. A job therefore runs
// only through event → route → Work → Execution.

const (
	deliveriesNS  = "schedule.deliveries"
	deliveriesKey = "cursor"
	ensureGap     = time.Minute
	passLimit     = 100
)

// The code-host delivery Events a job's `on.events` selects: the raw
// deliveries the GitHub extension accepts, read by their public schema.
const (
	typeDelivery     = "github.delivery"
	schemaDelivery   = "github.delivery.v1"
	maxDeliveryFiles = 1000
	maxPushCommits   = 2048
)

type deliveryPayload struct {
	Event    string `json:"event"`
	Action   string `json:"action,omitempty"`
	Delivery string `json:"delivery"`
	Ref      string `json:"ref,omitempty"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
	// Conclusion is how a finished check suite or run ended.
	Conclusion string   `json:"conclusion,omitempty"`
	Files      []string `json:"files,omitempty"`
	Commits    int      `json:"commits,omitempty"`
}

// source is a registered entry's ingress.
type source struct {
	name      string
	scope     domain.ID
	namespace string
	ingress   *Ingress
}

// Extension is the schedule extension.
type Extension struct {
	h inproc.Host

	mu         sync.Mutex
	entries    []Entry // the last configure's, enabled scopes' and disabled ones'
	public     string
	sources    map[string]*source // by namespace, for the daemon's lifetime
	lastEnsure time.Time
	codes      map[string]string // by task
}

// New returns the schedule extension.
func New() *Extension { return &Extension{sources: map[string]*source{}, codes: map[string]string{}} }

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Router     = (*Extension)(nil)
	_ inproc.WorkKinder = (*Extension)(nil)
	_ inproc.Recurring  = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Ingester   = (*Extension)(nil)
	_ inproc.Webhooker  = (*Extension)(nil)
	_ inproc.Tasker     = (*Extension)(nil)
	_ inproc.Checker    = (*Extension)(nil)
	_ inproc.Targeter   = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateExtension, Type: Type, Ingress: true, Validate: ValidateExtension, Describe: describeJobs},
		{Kind: protocol.ValidateRoute, Type: Type, Validate: func(c json.RawMessage, _ []string) error { return ValidateRoute(c) }, Describe: func(c json.RawMessage) []inproc.Entry {
			if d := DescribeRoute(c); d != "" {
				return []inproc.Entry{{Detail: d}}
			}
			return nil
		}},
	}
}

// describeJobs lists a schedule entry's jobs and when each runs. The entry
// was validated when its config loaded.
func describeJobs(c json.RawMessage) []inproc.Entry {
	var s ExtensionConfig
	if json.Unmarshal(c, &s) != nil {
		return nil
	}
	out := make([]inproc.Entry, len(s.Jobs))
	for i, j := range s.Jobs {
		out[i] = inproc.Entry{Name: j.Name, Detail: j.Describe()}
	}
	return out
}

func (*Extension) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{Type: Route{}}
}

// WorkKinds: job Work is standing, with the one-pending-run trigger
// selection, and timed, so a dry run starts no job.
func (*Extension) WorkKinds() []inproc.WorkKind {
	return []inproc.WorkKind{{Kind: WorkKind, Standing: true, Timed: true, Trigger: SelectTrigger}}
}

func (*Extension) Recurrences() map[string]inproc.RecurrenceFunc {
	return map[string]inproc.RecurrenceFunc{RecurrenceKind: Recurrence}
}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.h = h
	return nil
}

// Configure takes the generation's entries and reconciles the timers with
// them at once.
func (x *Extension) Configure(ctx context.Context, p protocol.ConfigureParams) {
	var entries []Entry
	add := func(es []protocol.ConfigEntry, enabled bool) {
		for _, e := range es {
			if e.Type != Type {
				continue
			}
			settings, err := ParseExtension(e.Config, e.Secrets)
			if err != nil || settings.Scope != e.ScopeID {
				continue
			}
			entries = append(entries, Entry{Scope: domain.ID(e.ScopeID), Enabled: enabled, Settings: settings, name: e.Name})
		}
	}
	add(p.Entries, true)
	add(p.Disabled, false)
	x.mu.Lock()
	x.entries, x.public = entries, p.PublicURL
	x.mu.Unlock()
	x.ensure(ctx, true)
}

// settings reads an entry's settings from the current generation, so job
// edits apply without re-registering the ingress.
func (x *Extension) settings(namespace string) func() (Settings, bool) {
	return func() (Settings, bool) {
		x.mu.Lock()
		defer x.mu.Unlock()
		for _, e := range x.entries {
			if e.Settings.Namespace == namespace && e.Enabled {
				return e.Settings, true
			}
		}
		return Settings{}, false
	}
}

// Sources are the enabled entries' webhook ingresses. A registration is
// immutable for the daemon's lifetime, so a namespace that moved to another
// entry needs a restart.
func (x *Extension) Sources() []inproc.Source {
	x.mu.Lock()
	defer x.mu.Unlock()
	restart := false
	for _, e := range x.entries {
		if !e.Enabled {
			continue
		}
		name := Type + "/" + string(e.Scope) + "/" + e.name
		if prev, ok := x.sources[e.Settings.Namespace]; ok {
			restart = restart || prev.name != name
			continue
		}
		x.sources[e.Settings.Namespace] = &source{name: name, scope: e.Scope, namespace: e.Settings.Namespace, ingress: NewIngress(e.Settings.Namespace, x.settings(e.Settings.Namespace), x.now)}
	}
	x.codes["sources"] = ""
	if restart {
		x.codes["sources"] = "restart_required"
	}
	x.reportLocked()
	out := make([]inproc.Source, 0, len(x.sources))
	for _, s := range x.sources {
		out = append(out, inproc.Source{Name: s.name, Source: extension.Source{Namespace: s.namespace, ScopeID: s.scope}, Ingress: s.ingress})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Webhook serves an entry's job webhooks: path is "<scope>/<entry>/<job>/
// <token>".
func (x *Extension) Webhook(path string) http.Handler {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 {
		return nil
	}
	mount := Type + "/" + parts[0] + "/" + parts[1]
	var src *source
	x.mu.Lock()
	for _, s := range x.sources {
		if s.name == mount {
			src = s
		}
	}
	x.mu.Unlock()
	if src == nil {
		return nil
	}
	push := func(ctx context.Context, d extension.Delivery) error { return x.h.Deliver(ctx, src.name, d) }
	inner := WebhookHandler(src.namespace, x.secret(src), x.settings(src.namespace), push, x.now)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = "/"+parts[2], ""
		inner.ServeHTTP(w, r2)
	})
}

// secret resolves an entry's webhook_secret in its scope.
func (x *Extension) secret(src *source) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		settings, ok := x.settings(src.namespace)()
		if !ok || settings.WebhookSecret == "" || x.h.Secrets == nil {
			return "", ErrCredential
		}
		values, err := x.h.Secrets(ctx, src.scope, "", []string{settings.WebhookSecret})
		if err != nil || values[settings.WebhookSecret] == "" {
			return "", ErrCredential
		}
		return values[settings.WebhookSecret], nil
	}
}

func (x *Extension) now() time.Time {
	if x.h.Now != nil {
		return x.h.Now()
	}
	return time.Now()
}

// Tasks: deliveries become job triggers and finished runs are announced on
// every pass; the timers get a repair pass every minute besides the one
// each configure makes.
func (x *Extension) Tasks() []inproc.Task {
	return []inproc.Task{
		{Name: "deliveries", Run: x.translate},
		{Name: "runs", Run: x.publish},
		{Name: "timers", Pace: ensureGap, Run: func(ctx context.Context) error { x.ensure(ctx, false); return nil }},
	}
}

// report records one task's code and reports them all, so one task's
// success never clears another's failure.
func (x *Extension) report(task, code string) {
	x.mu.Lock()
	x.codes[task] = code
	x.reportLocked()
	x.mu.Unlock()
}

func (x *Extension) reportLocked() {
	var codes []string
	for _, task := range []string{"timers", "deliveries", "runs", "sources"} {
		if c := x.codes[task]; c != "" {
			codes = append(codes, c)
		}
	}
	if x.h.Report != nil {
		x.h.Report(strings.Join(codes, ","))
	}
}

func (x *Extension) code(task string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.codes[task]
}

func (x *Extension) snapshot() []Entry {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]Entry(nil), x.entries...)
}

// ensure reconciles the schedule timers with the generation (each
// configure, and every minute as a repair pass). A dry run arms nothing.
func (x *Extension) ensure(ctx context.Context, force bool) {
	if x.h.DryRun || x.h.Timers == nil {
		return
	}
	x.mu.Lock()
	if !force && x.now().Sub(x.lastEnsure) < ensureGap {
		x.mu.Unlock()
		return
	}
	x.lastEnsure = x.now()
	x.mu.Unlock()
	res, err := Ensure(ctx, x.h.Store, x.now(), x.h.Timers, x.snapshot())
	code := strings.Join(res.Codes, ",")
	if err != nil {
		code = storageCode(err)
	}
	x.report("timers", code)
	if res.Armed+res.Rearmed+res.Cancelled > 0 {
		if x.h.Log != nil {
			x.h.Log.Info("schedule timers reconciled", "armed", res.Armed, "rearmed", res.Rearmed, "cancelled", res.Cancelled, "kept", res.Kept)
		}
		x.nudge()
	}
}

func (x *Extension) nudge() {
	if x.h.Nudge != nil {
		x.h.Nudge()
	}
}

func storageCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, storage.ErrConflict):
		return "conflict"
	}
	return "storage_failed"
}

// translate turns code-host deliveries into job triggers.
func (x *Extension) translate(ctx context.Context) error {
	entries := map[domain.ID][]Settings{}
	active := false
	for _, e := range x.snapshot() {
		if !e.Enabled {
			continue
		}
		for _, j := range e.Settings.Jobs {
			if j.On != nil && len(j.On.Events) > 0 {
				active = true
			}
		}
		entries[e.Scope] = append(entries[e.Scope], e.Settings)
	}
	_, codes, err := x.translateDeliveries(ctx, entries, active, passLimit)
	code := strings.Join(codes, ",")
	if err != nil && ctx.Err() == nil {
		code = storageCode(err)
	}
	x.report("deliveries", code)
	return nil
}

// translateDeliveries reads accepted code-host deliveries after a durable
// cursor and, for each job in the delivery's scope whose `on` selects it,
// accepts a derived schedule.event trigger (parent: the delivery), all in
// one transaction with the cursor. Replaying a delivery yields the same
// trigger identity, so a crash repeats nothing. When no job takes events
// the cursor still advances, so enabling one later does not replay old
// deliveries.
func (x *Extension) translateDeliveries(ctx context.Context, entries map[domain.ID][]Settings, active bool, limit int) (int, []string, error) {
	var examined int
	var codes []string
	now := x.now()
	err := x.h.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		examined, codes = 0, nil
		var cursor uint64
		if raw, err := tx.Get(deliveriesNS, deliveriesKey); err == nil {
			if json.Unmarshal(raw, &cursor) != nil {
				cursor = 0
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		events, err := tx.Events(cursor, limit)
		if err != nil {
			return err
		}
		for _, a := range events {
			examined++
			cursor = a.Sequence
			if !active || a.Event.Type != typeDelivery {
				continue
			}
			var p deliveryPayload
			if a.Event.Data.Schema != schemaDelivery || json.Unmarshal(a.Event.Data.Data, &p) != nil {
				continue
			}
			scope, err := x.h.EventScope(tx, a)
			if err != nil || len(entries[scope]) == 0 {
				continue
			}
			delivery := Delivery{EventID: a.Event.ID, ID: p.Delivery, Event: p.Event, Action: p.Action, Ref: p.Ref, Before: p.Before, SHA: p.After, Conclusion: p.Conclusion, Files: p.Files,
				Truncated: len(p.Files) >= maxDeliveryFiles || p.Commits >= maxPushCommits}
			for _, s := range entries[scope] {
				for _, j := range s.Jobs {
					if !j.Matches(delivery) {
						continue
					}
					trigger := EventTrigger(s.Namespace, j, delivery, a.Event)
					trigger.ReceivedAt = now
					if _, _, err := x.h.AcceptDerivedInTx(tx, scope, a.Event.ID, trigger); errors.Is(err, inproc.ErrCausalLimit) {
						codes = append(codes, "causal_rejected")
					} else if err != nil {
						return err
					}
				}
			}
		}
		raw, _ := json.Marshal(cursor)
		return tx.Put(deliveriesNS, deliveriesKey, raw)
	})
	return examined, codes, err
}

// publish announces each finished job run (05-02) as a derived Event of its
// trigger on the job's key, in one transaction with the job's checkpoint,
// so `job:` waits wake through ordinary dependency aliases and a crash
// repeats nothing.
func (x *Extension) publish(ctx context.Context) error {
	n, codes, err := x.publishRuns(ctx, passLimit)
	code := strings.Join(codes, ",")
	if err != nil && ctx.Err() == nil {
		code = storageCode(err)
	}
	x.report("runs", code)
	if n > 0 {
		x.nudge()
	}
	return nil
}

func (x *Extension) publishRuns(ctx context.Context, limit int) (int, []string, error) {
	var published int
	var codes []string
	now := x.now()
	err := x.h.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		published, codes = 0, nil
		ended, err := EndedRunsInTx(tx, now, limit)
		if err != nil {
			return err
		}
		for _, e := range ended {
			if e.Event.ID != "" {
				if _, _, err := x.h.AcceptDerivedInTx(tx, e.Scope, e.Parent, e.Event); errors.Is(err, inproc.ErrCausalLimit) {
					codes = append(codes, "causal_rejected")
				} else if err != nil {
					return err
				} else {
					published++
				}
			}
			if err := MarkEndedInTx(tx, e.Work, e.Attempt); err != nil {
				return err
			}
		}
		return nil
	})
	return published, codes, err
}

// Targets resolves `job:<name>` waits (05-02) against the current
// generation: the job's key in its schedule entry and the run-ended types.
func (x *Extension) Targets() map[string]func(context.Context, domain.ID, string) (domain.CorrelationKey, []string, bool) {
	return map[string]func(context.Context, domain.ID, string) (domain.CorrelationKey, []string, bool){"job": x.job}
}

func (x *Extension) job(_ context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
	for _, e := range x.snapshot() {
		if e.Scope != scope {
			continue
		}
		if _, ok := e.Settings.Job(name); ok {
			return domain.CorrelationKey{Source: e.Settings.Namespace, ExternalID: JobKey(name)}, append([]string(nil), RunTypes...), true
		}
	}
	return domain.CorrelationKey{}, nil, false
}

// Checks: each entry's jobs and their next timed slot, and (with secrets)
// each webhook job's full signed URL.
func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Run: x.check}, {Deep: true, Run: x.webhookCheck}}
}

// check reports each entry's jobs and their next timed slot. It makes no
// service calls.
func (x *Extension) check(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "schedule", Status: inproc.CheckOK, Code: "ok"}
	entries := x.snapshot()
	if len(entries) == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", "no schedule entries are configured"
		return check
	}
	timers := map[domain.ID]domain.Timer{}
	_ = x.h.Store.Transact(ctx, storage.Guard{Now: x.now()}, func(tx storage.Tx) error {
		all, err := tx.ListTimers()
		for _, t := range all {
			timers[t.ID] = t
		}
		return err
	})
	for _, task := range []string{"timers", "deliveries", "sources"} {
		if code := x.code(task); code != "" && check.Status == inproc.CheckOK {
			check.Status, check.Code = inproc.CheckWarn, code
		}
	}
	if x.h.DryRun {
		check.Detail = "dry run: no schedule timers are armed and no job starts"
	}
	for _, e := range entries {
		for _, j := range e.Settings.Jobs {
			item := e.Settings.Namespace + ": job " + j.Name
			var triggers []string
			if t, ok := timers[domain.ID(TimerID(e.Settings.Namespace, j.Name))]; ok && j.Recurring() {
				triggers = append(triggers, "next "+t.FireAt.UTC().Format(time.RFC3339))
			} else if j.Recurring() {
				triggers = append(triggers, "timer pending")
			}
			if j.On != nil && len(j.On.Events) > 0 {
				triggers = append(triggers, "events "+strings.Join(j.On.Events, ","))
			}
			if j.Webhook() {
				triggers = append(triggers, "webhook")
			}
			if len(triggers) == 0 {
				triggers = append(triggers, "manual only")
			}
			if !e.Enabled {
				triggers = append(triggers, "scope disabled")
			}
			check.Items = append(check.Items, item+": "+strings.Join(triggers, "; "))
		}
	}
	check.Items = append(check.Items, x.webhookItems()...)
	sort.Strings(check.Items)
	return check
}

// webhookItems describes each entry's webhook exposure.
func (x *Extension) webhookItems() []string {
	x.mu.Lock()
	sources := make([]*source, 0, len(x.sources))
	for _, s := range x.sources {
		sources = append(sources, s)
	}
	x.mu.Unlock()
	var out []string
	for _, src := range sources {
		settings, ok := x.settings(src.namespace)()
		if !ok {
			continue
		}
		for _, j := range settings.Jobs {
			if j.Webhook() {
				out = append(out, src.namespace+": job "+j.Name+" POST /"+src.name+"/"+j.Name+"/<token> (doctor --check-secrets prints the URL)")
			}
		}
	}
	return out
}

// webhookCheck (check_secrets only) prints each webhook job's full signed
// path; the token is a capability derived from the entry's secret.
func (x *Extension) webhookCheck(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "schedule_webhooks", Status: inproc.CheckOK, Code: "ok"}
	x.mu.Lock()
	sources := make([]*source, 0, len(x.sources))
	for _, s := range x.sources {
		sources = append(sources, s)
	}
	public := x.public
	x.mu.Unlock()
	for _, src := range sources {
		settings, ok := x.settings(src.namespace)()
		if !ok {
			continue
		}
		for _, j := range settings.Jobs {
			if !j.Webhook() {
				continue
			}
			secret, err := x.secret(src)(ctx)
			if err != nil {
				check.Status, check.Code = inproc.CheckFail, "credential_unavailable"
				check.Items = append(check.Items, fmt.Sprintf("%s: job %s: webhook secret unavailable", src.namespace, j.Name))
				continue
			}
			check.Items = append(check.Items, fmt.Sprintf("%s: job %s: POST %s/%s/%s/%s", src.namespace, j.Name, public, src.name, j.Name, Token(secret, src.namespace, j.Name)))
		}
	}
	if len(check.Items) == 0 {
		check.Status, check.Code = inproc.CheckInfo, "not_configured"
	}
	sort.Strings(check.Items)
	return check
}
