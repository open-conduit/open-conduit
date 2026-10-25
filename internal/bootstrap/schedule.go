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
	gh "github.com/alsiola/open-conduit/extensions/github"
	sc "github.com/alsiola/open-conduit/extensions/schedule"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
)

// Scheduled jobs (05-01). Each enabled scope's `schedule` entry registers a
// one-shot webhook ingress (custom POSTs at /schedule/<scope>/<entry>/<job>/
// <token> on the webhook listener); its timed jobs get durable core timers
// whose calendar recurrence and six-hour catch-up are the extension's
// RecurrenceFunc, reconciled with every committed generation; code-host
// deliveries a job's `on.events` selects become derived job triggers; and the
// `schedule` route makes every trigger input of the job's standing Work. A
// job therefore runs only through event → route → Work → Execution.

const (
	scheduleCursorNS  = "schedule.deliveries"
	scheduleCursorKey = "cursor"
	scheduleEnsureGap = time.Minute
)

type scheduleSource struct {
	name      string
	scope     domain.ID
	entry     string
	namespace string
}

type scheduleState struct {
	mu         sync.Mutex
	sources    map[string]*scheduleSource // by namespace
	last       sc.EnsureResult
	lastEnsure time.Time
}

// scheduleRegistry registers the extension type and the route handler's
// config validator (the handler itself is in handlers()).
func scheduleRegistry(reg *config.Registry) {
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	if reg.HandlerConfig == nil {
		reg.HandlerConfig = map[string]func(json.RawMessage) error{}
	}
	reg.Extensions[sc.Type] = config.ExtensionType{Ingress: true, Validate: sc.ValidateExtension}
	reg.HandlerConfig[sc.Type] = sc.ValidateRoute
}

// scheduleWorkKinds is the scheduler policy for job Work: standing, with the
// one-pending-run trigger selection.
func scheduleWorkKinds() map[string]runtime.WorkKindPolicy {
	return map[string]runtime.WorkKindPolicy{sc.WorkKind: {Standing: true, Trigger: sc.SelectTrigger}}
}

func scheduleRecurrences() map[string]runtime.RecurrenceFunc {
	return map[string]runtime.RecurrenceFunc{sc.RecurrenceKind: sc.Recurrence}
}

// scheduleEntries lists every schedule entry of the snapshot with its scope's
// effective enabled state.
func scheduleEntries(s *config.Snapshot) []sc.Entry {
	var out []sc.Entry
	if s == nil {
		return out
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil {
			continue
		}
		for _, x := range scope.Extensions {
			if x.Type != sc.Type {
				continue
			}
			settings, err := sc.ParseExtension(x.Config, x.SecretNames)
			if err != nil || settings.Scope != string(id) {
				continue
			}
			out = append(out, sc.Entry{Scope: id, Enabled: scope.Enabled, Settings: settings})
		}
	}
	return out
}

// scheduleSettings reads an entry's settings from the current generation, so
// job edits apply without re-registering the ingress.
func (d *daemon) scheduleSettings(namespace string) func() (sc.Settings, bool) {
	return func() (sc.Settings, bool) {
		snapshot, _ := d.configs.Current()
		for _, e := range scheduleEntries(snapshot) {
			if e.Settings.Namespace == namespace && e.Enabled {
				return e.Settings, true
			}
		}
		return sc.Settings{}, false
	}
}

// registerSchedule registers each enabled entry's webhook ingress once per
// runtime instance (registrations are immutable; a moved namespace needs a
// restart).
func (d *daemon) registerSchedule(ctx context.Context, s *config.Snapshot) {
	d.schedule.mu.Lock()
	defer d.schedule.mu.Unlock()
	if d.schedule.sources == nil {
		d.schedule.sources = map[string]*scheduleSource{}
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, x := range scope.Extensions {
			if x.Type != sc.Type {
				continue
			}
			settings, err := sc.ParseExtension(x.Config, x.SecretNames)
			if err != nil || settings.Scope != string(id) {
				continue
			}
			name := "schedule/" + string(id) + "/" + x.Name
			if prev, ok := d.schedule.sources[settings.Namespace]; ok {
				if prev.name != name {
					d.report("schedule", domain.ID(settings.Namespace), "restart_required")
				}
				continue
			}
			in := sc.NewIngress(settings.Namespace, d.scheduleSettings(settings.Namespace), d.now)
			interval := time.Duration(s.Settings.ReconcileSeconds) * time.Second
			if err := d.ingress.Register(ctx, name, extension.Source{Namespace: settings.Namespace, ScopeID: id}, in, interval); err != nil {
				d.report("schedule", domain.ID(settings.Namespace), errorCode(err))
				continue
			}
			d.schedule.sources[settings.Namespace] = &scheduleSource{name: name, scope: id, entry: x.Name, namespace: settings.Namespace}
			d.log.Info("schedule ingress registered", "name", name, "namespace", settings.Namespace)
		}
	}
}

// ensureSchedules reconciles the schedule timers with a generation (startup,
// every committed apply, and every minute as a repair pass). It takes the
// snapshot because OnApply runs under the config service's lock. A dry run
// arms nothing: v1 started no jobs in a dry run.
func (d *daemon) ensureSchedules(ctx context.Context, c *components, snapshot *config.Snapshot, force bool) {
	if d.o.DryRun {
		return
	}
	d.schedule.mu.Lock()
	if !force && d.now().Sub(d.schedule.lastEnsure) < scheduleEnsureGap {
		d.schedule.mu.Unlock()
		return
	}
	d.schedule.lastEnsure = d.now()
	d.schedule.mu.Unlock()
	res, err := sc.Ensure(ctx, d.coord, d.now(), c.timers, scheduleEntries(snapshot))
	code := strings.Join(res.Codes, ",")
	if err != nil {
		code = errorCode(err)
	}
	d.report("schedule_timers", "", code)
	d.schedule.mu.Lock()
	d.schedule.last = res
	d.schedule.mu.Unlock()
	if res.Armed+res.Rearmed+res.Cancelled > 0 {
		d.log.Info("schedule timers reconciled", "armed", res.Armed, "rearmed", res.Rearmed, "cancelled", res.Cancelled, "kept", res.Kept)
		d.poke()
	}
}

// translateSchedule turns code-host deliveries into job triggers.
func (d *daemon) translateSchedule(ctx context.Context) {
	snapshot, _ := d.configs.Current()
	entries := map[domain.ID][]sc.Settings{}
	active := false
	for _, e := range scheduleEntries(snapshot) {
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
	sources := map[string]domain.ID{}
	d.github.mu.Lock()
	for ns, src := range d.github.sources {
		sources[ns] = src.scope
	}
	d.github.mu.Unlock()
	limits := runtime.CausalLimits{}
	if snapshot != nil {
		limits = runtime.CausalLimits{MaxDepth: snapshot.Settings.CausalMaxDepth, PerMinute: snapshot.Settings.CausalPerMinute}
	}
	_, codes, err := translateDeliveries(ctx, d.coord, d.now(), limits, sources, entries, active, passLimit)
	code := strings.Join(codes, ",")
	if err != nil && ctx.Err() == nil {
		code = errorCode(err)
	}
	d.report("schedule_deliveries", "", code)
}

// translateDeliveries reads accepted raw GitHub deliveries after a durable
// cursor and, for each job in the delivery's scope whose `on` selects it,
// accepts a derived schedule.event trigger (parent: the delivery), all in one
// transaction with the cursor. Replaying a delivery yields the same trigger
// identity, so a crash repeats nothing. When no job takes events the cursor
// still advances, so enabling one later does not replay old deliveries.
func translateDeliveries(ctx context.Context, store storage.Store, now time.Time, limits runtime.CausalLimits, sources map[string]domain.ID, entries map[domain.ID][]sc.Settings, active bool, limit int) (int, []string, error) {
	var examined int
	var codes []string
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		examined, codes = 0, nil
		var cursor uint64
		if raw, err := tx.Get(scheduleCursorNS, scheduleCursorKey); err == nil {
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
			if !active || a.Event.Type != gh.TypeDelivery {
				continue
			}
			scope, ok := sources[a.Event.Source]
			if !ok {
				continue
			}
			var p gh.DeliveryPayload
			if a.Event.Data.Schema != gh.SchemaDelivery || json.Unmarshal(a.Event.Data.Data, &p) != nil {
				continue
			}
			delivery := sc.Delivery{EventID: a.Event.ID, ID: p.Delivery, Event: p.Event, Action: p.Action, Ref: p.Ref, Before: p.Before, SHA: p.After, Files: p.Files,
				Truncated: len(p.Files) >= gh.MaxDeliveryFiles || p.Commits >= gh.MaxPushCommits}
			for _, s := range entries[scope] {
				for _, j := range s.Jobs {
					if !j.Matches(delivery) {
						continue
					}
					trigger := sc.EventTrigger(s.Namespace, j, delivery, a.Event)
					trigger.ReceivedAt = now
					if _, _, err := runtime.AcceptDerivedInTx(tx, now, limits, scope, a.Event.ID, trigger); errors.Is(err, runtime.ErrCausalRejected) {
						codes = append(codes, "causal_rejected")
					} else if err != nil {
						return err
					}
				}
			}
		}
		raw, _ := json.Marshal(cursor)
		return tx.Put(scheduleCursorNS, scheduleCursorKey, raw)
	})
	return examined, codes, err
}

// scheduleWebhook returns the webhook handler of the entry mounted at the
// start of path ("schedule/<scope>/<entry>/<job>/<token>") with the mount
// stripped, or nil.
func (d *daemon) scheduleWebhook(path string) (http.Handler, string) {
	parts := strings.SplitN(path, "/", 4)
	if len(parts) != 4 {
		return nil, ""
	}
	mount := strings.Join(parts[:3], "/")
	var src *scheduleSource
	d.schedule.mu.Lock()
	for _, s := range d.schedule.sources {
		if s.name == mount {
			src = s
		}
	}
	d.schedule.mu.Unlock()
	if src == nil {
		return nil, ""
	}
	push := func(ctx context.Context, delivery extension.Delivery) error {
		_, err := d.ingress.Push(ctx, src.name, delivery)
		if err == nil {
			d.poke()
		}
		return err
	}
	return sc.WebhookHandler(src.namespace, d.scheduleSecret(src), d.scheduleSettings(src.namespace), push, d.now), "/" + parts[3]
}

// scheduleSecret resolves an entry's webhook_secret from its scope's
// effective references in the current generation.
func (d *daemon) scheduleSecret(src *scheduleSource) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		settings, ok := d.scheduleSettings(src.namespace)()
		snapshot, _ := d.configs.Current()
		if !ok || settings.WebhookSecret == "" || snapshot == nil || snapshot.Scopes[src.scope] == nil {
			return "", sc.ErrCredential
		}
		values, err := d.resolver.ResolveNames(ctx, snapshot.Scopes[src.scope], []string{settings.WebhookSecret})
		if err != nil {
			return "", sc.ErrCredential
		}
		return values[settings.WebhookSecret], nil
	}
}

// scheduleWebhookItems describes each entry's webhook exposure for doctor.
func (d *daemon) scheduleWebhookItems() []string {
	d.schedule.mu.Lock()
	defer d.schedule.mu.Unlock()
	var out []string
	for ns, src := range d.schedule.sources {
		settings, ok := d.scheduleSettings(ns)()
		if !ok {
			continue
		}
		for _, j := range settings.Jobs {
			if j.Webhook() {
				out = append(out, ns+": job "+j.Name+" POST /"+src.name+"/"+j.Name+"/<token> (doctor --check-secrets prints the URL)")
			}
		}
	}
	return out
}

// scheduleCheck reports each entry's jobs and their next timed slot. It
// makes no service calls.
func (d *daemon) scheduleCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "schedule", Status: runtime.CheckOK, Code: "ok"}
	snapshot, _ := d.configs.Current()
	entries := scheduleEntries(snapshot)
	if len(entries) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no schedule entries are configured"
		return check
	}
	timers := map[domain.ID]domain.Timer{}
	_ = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		all, err := tx.ListTimers()
		for _, t := range all {
			timers[t.ID] = t
		}
		return err
	})
	for _, code := range []string{d.reportedCode("schedule_timers", ""), d.reportedCode("schedule_deliveries", "")} {
		if code != "" && check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckWarn, code
		}
	}
	if d.o.DryRun {
		check.Detail = "dry run: no schedule timers are armed and no job starts"
	}
	for _, e := range entries {
		for _, j := range e.Settings.Jobs {
			item := e.Settings.Namespace + ": job " + j.Name
			var triggers []string
			if t, ok := timers[domain.ID(sc.TimerID(e.Settings.Namespace, j.Name))]; ok && j.Recurring() {
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
	sort.Strings(check.Items)
	return check
}

// scheduleWebhookCheck (check_secrets only) prints each webhook job's full
// signed path; the token is a capability derived from the entry's secret.
func (d *daemon) scheduleWebhookCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "schedule_webhooks", Status: runtime.CheckOK, Code: "ok"}
	d.schedule.mu.Lock()
	sources := make([]*scheduleSource, 0, len(d.schedule.sources))
	for _, s := range d.schedule.sources {
		sources = append(sources, s)
	}
	d.schedule.mu.Unlock()
	d.hooks.mu.Lock()
	public := d.hooks.public
	d.hooks.mu.Unlock()
	for _, src := range sources {
		settings, ok := d.scheduleSettings(src.namespace)()
		if !ok {
			continue
		}
		for _, j := range settings.Jobs {
			if !j.Webhook() {
				continue
			}
			secret, err := d.scheduleSecret(src)(ctx)
			if err != nil {
				check.Status, check.Code = runtime.CheckFail, "credential_unavailable"
				check.Items = append(check.Items, fmt.Sprintf("%s: job %s: webhook secret unavailable", src.namespace, j.Name))
				continue
			}
			check.Items = append(check.Items, fmt.Sprintf("%s: job %s: POST %s/%s/%s/%s", src.namespace, j.Name, public, src.name, j.Name, sc.Token(secret, src.namespace, j.Name)))
		}
	}
	if len(check.Items) == 0 {
		check.Status, check.Code = runtime.CheckInfo, "not_configured"
	}
	sort.Strings(check.Items)
	return check
}

// scheduleJob resolves a `job:<name>` wait (05-02) against the current
// generation: the job's key in its schedule entry and the run-ended types.
func (d *daemon) scheduleJob(_ context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
	snapshot, _ := d.configs.Current()
	for _, e := range scheduleEntries(snapshot) {
		if e.Scope != scope {
			continue
		}
		if _, ok := e.Settings.Job(name); ok {
			return domain.CorrelationKey{Source: e.Settings.Namespace, ExternalID: sc.JobKey(name)}, append([]string(nil), sc.RunTypes...), true
		}
	}
	return domain.CorrelationKey{}, nil, false
}

// publishJobRuns announces each finished job run (05-02) as a derived Event
// of its trigger on the job's key, in one transaction with the job's
// checkpoint, so `job:` waits wake through ordinary dependency aliases and a
// crash repeats nothing.
func (d *daemon) publishJobRuns(ctx context.Context) {
	snapshot, _ := d.configs.Current()
	limits := runtime.CausalLimits{}
	if snapshot != nil {
		limits = runtime.CausalLimits{MaxDepth: snapshot.Settings.CausalMaxDepth, PerMinute: snapshot.Settings.CausalPerMinute}
	}
	n, codes, err := publishJobRuns(ctx, d.coord, d.now(), limits, passLimit)
	code := strings.Join(codes, ",")
	if err != nil && ctx.Err() == nil {
		code = errorCode(err)
	}
	d.report("schedule_runs", "", code)
	if n > 0 {
		d.poke()
	}
}

func publishJobRuns(ctx context.Context, store storage.Store, now time.Time, limits runtime.CausalLimits, limit int) (int, []string, error) {
	var published int
	var codes []string
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		published, codes = 0, nil
		ended, err := sc.EndedRunsInTx(tx, now, limit)
		if err != nil {
			return err
		}
		for _, e := range ended {
			if e.Event.ID != "" {
				if _, _, err := runtime.AcceptDerivedInTx(tx, now, limits, e.Scope, e.Parent, e.Event); errors.Is(err, runtime.ErrCausalRejected) {
					codes = append(codes, "causal_rejected")
				} else if err != nil {
					return err
				} else {
					published++
				}
			}
			if err := sc.MarkEndedInTx(tx, e.Work, e.Attempt); err != nil {
				return err
			}
		}
		return nil
	})
	return published, codes, err
}
