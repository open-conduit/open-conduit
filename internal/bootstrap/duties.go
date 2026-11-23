package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Every periodic thing the daemon does is listed in hostDuties, and each
// place in this package that paces one carries a `//duty:<name>` comment
// (TestEveryPeriodicDutyIsListed). A duty is one of two kinds:
//
//   - Durable: its next due time is in storage, so it survives a restart and
//     is part of when the daemon next needs to be awake.
//   - Process-bound: it has no obligation of its own while the daemon is
//     stopped, and Why says what makes that true.

// dutyNamespace holds each durable duty's dutyRecord, by name.
const dutyNamespace = "core.host.duties"

// Durable duties.
const (
	dutyRetention = "retention"
	dutySweep     = "workspace_sweep"
	dutyBackup    = "backup"
	dutyShipment  = "backup_shipment"
)

// retentionSlack is how late retention may run while the daemon is stopped.
// Nothing it deletes is needed sooner: what it removes is already expired.
const retentionSlack = 6 * time.Hour

// hostDuty is one periodic duty of the daemon.
type hostDuty struct {
	Name string
	// Durable duties keep a next due time in storage. Applies says whether
	// the running configuration has the duty at all (nil: always); one that
	// doesn't apply owes nothing.
	Durable bool
	Applies func(*daemon) bool
	// Slack is how long a durable duty may wait past its due time for a
	// daemon that is stopped. An awake daemon runs it when due; only
	// due+Slack brings the next wake forward, so a ten-minute sweep doesn't
	// wake a sleeping laptop every ten minutes.
	Slack time.Duration
	// WhenOwed marks a duty that exists only while something is owed: a
	// zero due time means nothing is.
	WhenOwed bool
	// Why is why a process-bound duty needs no process while stopped.
	Why string
}

var hostDuties = []hostDuty{
	{Name: dutyRetention, Durable: true, Slack: retentionSlack},
	{Name: dutySweep, Durable: true, Applies: func(d *daemon) bool { return len(d.resources.providers.all()) > 0 }},
	{Name: dutyBackup, Durable: true, Applies: func(d *daemon) bool { return d.o.BackupInterval >= 0 && d.backups.db != nil }},
	{Name: dutyShipment, Durable: true, WhenOwed: true, Applies: func(d *daemon) bool { return d.o.BackupInterval >= 0 && d.backups.db != nil }},
	{Name: "host_loop", Why: "drives the other duties; each one's due time is durable or re-derived at start"},
	{Name: "lease_renewal", Why: "only while an attempt runs, which is itself a reason not to stop"},
	{Name: "instance_renewal", Why: "only on shared state, where a stopped daemon's lease is meant to lapse so a standby can take over"},
	{Name: "instance_standby", Why: "a standby asking for the lease; a stopped one asks again when it starts"},
	{Name: "resource_reconcile", Why: "re-derived from durable leases on the first pass after a start"},
	{Name: "container_reconcile", Why: "re-derived from durable attempts on the first pass after a start"},
	{Name: "package_drain", Why: "retiring packages exist only in a running process"},
	{Name: "package_retry", Why: "retries starting a package in this process; a new start tries again"},
	{Name: "package_idle", Why: "stops an idle on-demand package process; a stopped daemon has none"},
	{Name: "package_task", Why: "a task that hands its waiting back has a durable due time, an obligation in the answer; one on a bare interval runs on the first pass after a start and is listed with its pace"},
	{Name: "executor_state_sweep", Why: "runs on the first pass after a start"},
	{Name: "tunnel_probe", Why: "probes this process's public URL"},
	{Name: "forward_report", Why: "throttles a log line"},
	{Name: "metric_export", Why: "exports this process's meter"},
	{Name: "usage_heartbeat", Why: "reports on this process while it runs"},
	{Name: "dev_watch", Why: "belongs to the dev command, not a served daemon"},
	{Name: "shipment_stall", Why: "bounds an upload in flight, which is itself a reason not to stop"},
	{Name: "tunnel_restart", Why: "restarts this process's tunnel child"},
	{Name: "terminal_hangup", Why: "ends an operator's terminal on this process"},
	{Name: "database_rebuild", Why: "tried only at a start, from a time kept in storage; a stopped daemon owes nothing"},
}

// dutyRecord is a durable duty's schedule. Deferred is when an overdue duty
// was first put off at a start, so repeated starts can't put it off for ever.
type dutyRecord struct {
	NextDue  time.Time `json:"next_due"`
	LastRun  time.Time `json:"last_run,omitzero"`
	Deferred time.Time `json:"deferred,omitzero"`
}

// dutyClock is the durable schedules, read through to storage on write. The
// loop consults the copy so an idle pass opens no transaction for them.
type dutyClock struct {
	mu      sync.Mutex
	records map[string]dutyRecord
}

// loadDuty is the duty's stored schedule, or at when it has none. A stored
// time further off than interval is brought back to now+interval, so a
// shortened interval takes effect.
func (d *daemon) loadDuty(ctx context.Context, name string, at time.Time, interval time.Duration) (dutyRecord, error) {
	r := dutyRecord{NextDue: at}
	if d.coord != nil {
		err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
			raw, e := tx.Get(dutyNamespace, name)
			if errors.Is(e, storage.ErrNotFound) {
				return nil
			}
			if e != nil {
				return e
			}
			return json.Unmarshal(raw, &r)
		})
		if err != nil {
			return r, err
		}
	}
	if latest := d.now().Add(interval); interval > 0 && r.NextDue.After(latest) {
		r.NextDue = latest
	}
	d.duties.mu.Lock()
	defer d.duties.mu.Unlock()
	if d.duties.records == nil {
		d.duties.records = map[string]dutyRecord{}
	}
	d.duties.records[name] = r
	return r, nil
}

// dutyDue reports whether the duty's time has come. A duty never loaded is
// due, so a daemon that couldn't read its schedule errs towards running it.
func (d *daemon) dutyDue(name string) bool {
	d.duties.mu.Lock()
	defer d.duties.mu.Unlock()
	r, ok := d.duties.records[name]
	return !ok || !d.now().Before(r.NextDue)
}

// setDuty stores the duty's schedule. A failed write is reported and keeps
// the new time in memory: the duty stays paced, and at worst runs early after
// a restart.
func (d *daemon) setDuty(ctx context.Context, name string, r dutyRecord) {
	d.duties.mu.Lock()
	if d.duties.records == nil {
		d.duties.records = map[string]dutyRecord{}
	}
	d.duties.records[name] = r
	d.duties.mu.Unlock()
	if d.coord == nil {
		return
	}
	raw, err := json.Marshal(r)
	if err == nil {
		err = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
			return tx.Put(dutyNamespace, name, raw)
		})
	}
	if err != nil && ctx.Err() == nil {
		d.report("duty_schedule", "", errorCode(err))
		return
	}
	d.clear("duty_schedule", "")
}

// dutyRan records a completed run and the next one's time.
func (d *daemon) dutyRan(ctx context.Context, name string, next time.Time) {
	d.setDuty(ctx, name, dutyRecord{NextDue: next, LastRun: d.now()})
}

// startupNamespace holds the latest start's timing, under startupKey.
const (
	startupNamespace = "core.host.startup"
	startupKey       = "latest"
)

// startupClock is this start's timing; recorded is set once it is stored.
type startupClock struct {
	began, listening time.Time
	recorded         bool
}

// recordStartup stores what this start cost, once, after its first pass.
func (d *daemon) recordStartup(ctx context.Context) {
	s := &d.startup
	if s.recorded || s.began.IsZero() {
		return
	}
	s.recorded = true
	now := d.now()
	raw, err := json.Marshal(management.QuiescenceStartup{At: s.began.UTC(), ListeningMS: s.listening.Sub(s.began).Milliseconds(), FirstPassMS: now.Sub(s.began).Milliseconds()})
	if err == nil {
		err = d.coord.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			return tx.Put(startupNamespace, startupKey, raw)
		})
	}
	if err != nil && ctx.Err() == nil {
		d.report("duty_schedule", "", errorCode(err))
	}
}

// quiescence is GET /quiescence, built from storage: core's in-flight state,
// its timers and sources, and the durable duties. Only what is running right
// now (a backup, a retention pass) comes from this process, since that is
// the one thing storage can't say.
func (d *daemon) quiescence(ctx context.Context) (management.Quiescence, error) {
	q := management.Quiescence{Reasons: []runtime.QuiescenceReason{}, Obligations: []management.QuiescenceObligation{}, ProcessBound: []management.QuiescenceDuty{}, PackageTasks: []management.QuiescenceTask{}}
	var timer time.Time
	stored := map[string]dutyRecord{}
	sleeps := map[string]taskSleep{}
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		clear(stored)
		clear(sleeps)
		q.Startup = nil
		var e error
		if q.Reasons, timer, e = runtime.QuiescenceInTx(tx); e != nil {
			return e
		}
		rows, e := tx.List(dutyNamespace, "")
		if e != nil {
			return e
		}
		for _, row := range rows {
			var r dutyRecord
			if e = json.Unmarshal(row.Value, &r); e != nil {
				return e
			}
			stored[row.Key] = r
		}
		if rows, e = tx.List(taskNamespace, ""); e != nil {
			return e
		}
		for _, row := range rows {
			var r taskSleep
			if json.Unmarshal(row.Value, &r) == nil {
				sleeps[row.Key] = r
			}
		}
		raw, e := tx.Get(startupNamespace, startupKey)
		if errors.Is(e, storage.ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		q.Startup = &management.QuiescenceStartup{}
		return json.Unmarshal(raw, q.Startup)
	})
	if err != nil {
		return q, err
	}
	if q.Reasons == nil {
		q.Reasons = []runtime.QuiescenceReason{}
	}
	if !timer.IsZero() {
		q.Obligations = append(q.Obligations, management.QuiescenceObligation{Duty: "timer", Due: timer, Latest: timer})
	}
	if d.ingress != nil {
		sources, err := d.ingress.Inspect(ctx)
		if err != nil {
			return q, err
		}
		for _, s := range sources {
			q.Obligations = append(q.Obligations, management.QuiescenceObligation{Duty: "ingress:" + s.Name, Due: s.NextDue, Latest: s.NextDue})
		}
	}
	for _, duty := range hostDuties {
		if !duty.Durable {
			q.ProcessBound = append(q.ProcessBound, management.QuiescenceDuty{Duty: duty.Name, Why: duty.Why})
			continue
		}
		if duty.Applies != nil && !duty.Applies(d) {
			continue
		}
		if duty.WhenOwed && stored[duty.Name].NextDue.IsZero() {
			continue
		}
		// A durable duty with no record yet is due: it hasn't been scheduled.
		q.Obligations = append(q.Obligations, management.QuiescenceObligation{Duty: duty.Name, Due: stored[duty.Name].NextDue, Latest: stored[duty.Name].NextDue.Add(duty.Slack)})
	}
	for _, x := range d.extensionPackages() {
		for _, t := range x.Tasks() {
			key := x.Name() + "/" + t.Name
			entry := management.QuiescenceTask{Task: key, EverySeconds: int64(t.Pace / time.Second)}
			if r, ok := sleeps[key]; ok {
				due := r.NextDue
				entry.NextDue, entry.Sleeping = &due, true
				q.Obligations = append(q.Obligations, management.QuiescenceObligation{Duty: "task:" + key, Due: due, Latest: due})
			}
			q.PackageTasks = append(q.PackageTasks, entry)
		}
	}
	sort.Slice(q.PackageTasks, func(i, j int) bool { return q.PackageTasks[i].Task < q.PackageTasks[j].Task })
	sort.SliceStable(q.Obligations, func(i, j int) bool { return q.Obligations[i].Latest.Before(q.Obligations[j].Latest) })
	q.Reasons = append(q.Reasons, d.passesInProgress()...)
	now := d.now()
	overdue := 0
	for _, o := range q.Obligations {
		if !o.Latest.After(now) {
			overdue++
		}
	}
	if overdue > 0 {
		q.Reasons = append(q.Reasons, runtime.QuiescenceReason{Code: "duty_due", Count: overdue})
	}
	sort.Slice(q.Reasons, func(i, j int) bool { return q.Reasons[i].Code < q.Reasons[j].Code })
	q.Quiescent = len(q.Reasons) == 0
	if q.Quiescent && len(q.Obligations) > 0 {
		first := q.Obligations[0]
		q.Until, q.Next = &first.Latest, first.Duty
	}
	return q, nil
}

// passesInProgress are the host's own passes running at this moment.
func (d *daemon) passesInProgress() []runtime.QuiescenceReason {
	var out []runtime.QuiescenceReason
	add := func(code string, on bool) {
		if on {
			out = append(out, runtime.QuiescenceReason{Code: code, Count: 1})
		}
	}
	add("retention_in_progress", d.retaining.Load())
	d.backups.mu.Lock()
	add("backup_in_progress", d.backups.running)
	add("backup_shipping", d.backups.shipping != "")
	d.backups.mu.Unlock()
	d.resources.mu.Lock()
	add("workspace_sweep_in_progress", d.resources.sweeping)
	d.resources.mu.Unlock()
	return out
}
