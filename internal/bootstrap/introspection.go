package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	is "github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/extensions/introspection/host"
	"github.com/open-conduit/open-conduit/extensions/posthog"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Introspection (03-08 reporting; 05-03 sweep, triage, fixes, host watch and
// telemetry). The extension is composed like any other first-party
// extension, and every feature hangs off the current generation's
// `introspection` entries:
//
//   - its entry type and route config validator are registered with the
//     config registry and its route handler with the router; its operations
//     and Event families go on a generation's toolbelt only when that
//     generation has an enabled scope with an entry;
//   - its standing sweep/triage Work kinds are a scheduler WorkKinds policy
//     and its calendar a timer recurrence, both generic host options; its
//     timers are reconciled with every committed generation (Ensure);
//   - Work the configuration no longer has a use for is retired by the
//     extension's removal policy (is.Retired) through ordinary operator
//     cancellation;
//   - the optional host watch is a host pass that reports crossings as
//     extension-originated reports, and telemetry events are a lifecycle
//     exporter plus a doctor check, all on public observability hooks.
//
// Removing or disabling the entries therefore removes the features without
// any change to core. The executors are whatever bindings the user's
// `introspection` routes name: none is built in.

const introspectionEnsureGap = time.Minute

type introspectionState struct {
	mu         sync.Mutex
	lastEnsure time.Time
	last       is.EnsureResult
	retired    int
	watcher    is.HostWatcher
	lastRead   time.Time
	readErr    string
	sink       is.Sink
	file       *os.File
	// telemetry mirrors whether the current generation turns telemetry on,
	// for the exporter's pipeline goroutine.
	telemetry atomic.Bool
}

// introspectionRegistry registers the extension type and the route handler's
// config validator (the handler itself is in handlers()).
func introspectionRegistry(reg *config.Registry) {
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	if reg.HandlerConfig == nil {
		reg.HandlerConfig = map[string]func(json.RawMessage) error{}
	}
	reg.Extensions[is.Type] = config.ExtensionType{Validate: is.ValidateExtension}
	reg.HandlerConfig[is.Type] = is.ValidateRoute
	reg.Operations = append(reg.Operations, is.Operations()...)
}

// workKinds is the scheduler's Work kind policy: scheduled jobs and the
// standing introspection runs.
func workKinds() map[string]runtime.WorkKindPolicy {
	kinds := scheduleWorkKinds()
	kinds[is.WorkKindSweep] = runtime.WorkKindPolicy{Standing: true}
	kinds[is.WorkKindTriage] = runtime.WorkKindPolicy{Standing: true}
	return kinds
}

// recurrences are the timer recurrences: scheduled jobs and the
// introspection calendar.
func recurrences() map[string]runtime.RecurrenceFunc {
	r := scheduleRecurrences()
	r[is.RecurrenceKind] = is.Recurrence
	return r
}

// introspectionEntries lists every scope's entry with the scope's effective
// enabled state.
func introspectionEntries(s *config.Snapshot) []is.Entry {
	var out []is.Entry
	if s == nil {
		return out
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil {
			continue
		}
		for _, x := range scope.Extensions {
			if x.Type != is.Type {
				continue
			}
			cfg, err := is.ParseConfig(x.Config)
			if err != nil {
				continue
			}
			out = append(out, is.Entry{Scope: id, Enabled: scope.Enabled, Config: cfg})
			break
		}
	}
	return out
}

// introspectionEnabled reports whether any enabled scope of the generation
// has an `introspection` entry.
func introspectionEnabled(s *config.Snapshot) bool {
	for _, e := range introspectionEntries(s) {
		if e.Enabled {
			return true
		}
	}
	return false
}

// registeredOnly narrows a binding's granted operations to those registered
// on the generation's toolbelt, so a binding that still lists the operations
// of a disabled extension keeps running without them (its calls are
// undeclared) instead of failing to obtain a token.
func registeredOnly(granted, registered []string) []string {
	known := map[string]bool{}
	for _, op := range registered {
		known[op] = true
	}
	out := make([]string, 0, len(granted))
	for _, op := range granted {
		if known[op] {
			out = append(out, op)
		}
	}
	return out
}

// ensureIntrospection reconciles the sweep/triage timers with a generation
// and retires Work it no longer has a use for (startup, every committed
// apply, and every minute as a repair pass). A dry run arms nothing, as for
// scheduled jobs.
func (d *daemon) ensureIntrospection(ctx context.Context, c *components, snapshot *config.Snapshot, force bool) {
	d.introspection.mu.Lock()
	if !force && d.now().Sub(d.introspection.lastEnsure) < introspectionEnsureGap {
		d.introspection.mu.Unlock()
		return
	}
	d.introspection.lastEnsure = d.now()
	d.introspection.mu.Unlock()
	if !d.o.DryRun {
		res, err := is.Ensure(ctx, d.coord, d.now(), c.timers, introspectionEntries(snapshot))
		code := strings.Join(res.Codes, ",")
		if err != nil {
			code = errorCode(err)
		}
		d.report("introspection_timers", "", code)
		d.introspection.mu.Lock()
		d.introspection.last = res
		d.introspection.mu.Unlock()
		if res.Armed+res.Rearmed+res.Cancelled > 0 {
			d.log.Info("introspection timers reconciled", "armed", res.Armed, "rearmed", res.Rearmed, "cancelled", res.Cancelled, "kept", res.Kept)
			d.poke()
		}
	}
	d.retireIntrospection(ctx, c, snapshot)
}

// retireIntrospection applies the removal policy: Work of the extension's
// kinds the configuration no longer has a use for is cancelled, with a
// recorded reason, once no attempt of it is running.
func (d *daemon) retireIntrospection(ctx context.Context, c *components, snapshot *config.Snapshot) {
	if snapshot == nil || c.control == nil {
		return
	}
	configured := map[domain.ID]bool{}
	for _, id := range snapshot.Order {
		configured[id] = false
	}
	for _, e := range introspectionEntries(snapshot) {
		configured[e.Scope] = true
	}
	var retired []domain.Work
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		var err error
		retired, err = is.Retired(tx, configured)
		return err
	})
	code := ""
	if err != nil {
		code = errorCode(err)
	}
	for _, w := range retired {
		key := "introspection-retire/" + string(w.ID) + "/" + fmt.Sprint(w.Revision)
		if _, err := c.control.CancelWork(ctx, w.ID, w.Revision, is.RetireReason, key); err != nil && !errors.Is(err, runtime.ErrRevision) && !errors.Is(err, storage.ErrConflict) {
			code = errorCode(err)
			continue
		}
		d.log.Info("introspection work retired", "work", string(w.ID), "kind", w.Kind)
		d.introspection.mu.Lock()
		d.introspection.retired++
		d.introspection.mu.Unlock()
	}
	d.report("introspection_retire", "", code)
	if len(retired) > 0 {
		d.poke()
	}
}

// hostWatch takes a reading when one is due, reports each threshold crossed
// over as an extension-originated report into every enabled scope whose
// entry watches the host, and captures the reading as telemetry when an
// entry turns telemetry on.
func (d *daemon) hostWatch(ctx context.Context) {
	snapshot, _ := d.configs.Current()
	var watched []is.Entry
	interval := time.Duration(0)
	var disks []string
	for _, e := range introspectionEntries(snapshot) {
		if !e.Enabled || e.Config.HostWatch == nil {
			continue
		}
		watched = append(watched, e)
		if i := e.Config.HostWatch.Interval(); interval == 0 || i < interval {
			interval = i
		}
		for _, p := range e.Config.HostWatch.Disks {
			if strings.HasPrefix(p, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					p = filepath.Join(home, p[2:])
				}
			}
			disks = append(disks, p)
		}
	}
	if len(watched) == 0 {
		return
	}
	d.introspection.mu.Lock()
	due := d.now().Sub(d.introspection.lastRead) >= interval
	if due {
		d.introspection.lastRead = d.now()
	}
	d.introspection.mu.Unlock()
	if !due {
		return
	}
	sample, err := host.Read(append([]string{d.o.StateDir}, disks...))
	d.introspection.mu.Lock()
	d.introspection.readErr = ""
	if err != nil {
		d.introspection.readErr = "unreadable"
	}
	d.introspection.mu.Unlock()
	if err != nil {
		d.report("host_watch", "", "unreadable")
		return
	}
	at := d.now()
	telemetry := d.introspectionTelemetry()
	if telemetry {
		_ = d.introspection.sink.Capture(ctx, is.EventHostMetrics, at, is.HostProps(sample))
	}
	// One watcher for the machine; the first watching entry's thresholds
	// decide (a machine has one set of readings).
	alerts := d.introspection.watcher.Update(sample, *watched[0].Config.HostWatch)
	code := ""
	for _, a := range alerts {
		d.log.Info(a.String())
		if telemetry {
			props := map[string]any{"metric": a.Metric, "value": a.Value, "threshold": a.Threshold, "over": a.Over}
			if a.Path != "" {
				props["path"] = a.Path
			}
			_ = d.introspection.sink.Capture(ctx, is.EventHostAlert, at, props)
		}
		if !a.Over {
			continue
		}
		for _, e := range watched {
			ev, rep := is.HostReport(e.Scope, a, at)
			if err := d.coord.Transact(ctx, storage.Guard{Now: at}, func(tx storage.Tx) error {
				accepted, added, err := runtime.AcceptInTx(tx, e.Scope, ev)
				if err != nil || !added {
					return err
				}
				return is.PutOriginInTx(tx, accepted, rep)
			}); err != nil {
				code = errorCode(err)
			}
		}
		d.poke()
	}
	d.report("host_watch", "", code)
}

// introspectionTelemetryOn reports whether an enabled entry of the
// generation turns telemetry on.
func introspectionTelemetryOn(s *config.Snapshot) bool {
	for _, e := range introspectionEntries(s) {
		if e.Enabled && e.Config.Telemetry {
			return true
		}
	}
	return false
}

// introspectionTelemetry reports whether telemetry is on and its sink open.
func (d *daemon) introspectionTelemetry() bool {
	return d.introspection.sink != nil && d.introspection.telemetry.Load()
}

// openIntrospectionTelemetry opens the telemetry events log the exporter and
// host watch write (logs/introspection-telemetry.jsonl).
func (d *daemon) openIntrospectionTelemetry() (*is.Exporter, error) {
	path := filepath.Join(filepath.Dir(d.o.LifecycleLog), "introspection-telemetry.jsonl")
	f, err := openLog(path)
	if err != nil {
		return nil, err
	}
	// The events log is the primary sink; enabled PostHog entries that take
	// introspection telemetry receive the same events (06-01).
	d.introspection.file = f
	d.introspection.sink = &posthog.Fanout{Primary: is.NewJSONSink(f), Targets: d.posthogTargets, Diagnostic: func(code string) { d.report("posthog", "", code) }}
	return &is.Exporter{Sink: d.introspection.sink, Enabled: d.introspectionTelemetry}, nil
}

func (d *daemon) closeIntrospectionTelemetry() {
	if d.introspection.file != nil {
		_ = d.introspection.file.Close()
	}
}

// introspectionCheck reports each entry's features: the sweep/triage
// schedules with their next slot, the host watch's readings over threshold
// and telemetry. It makes no service calls.
func (d *daemon) introspectionCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "introspection", Status: runtime.CheckOK, Code: "ok"}
	snapshot, _ := d.configs.Current()
	entries := introspectionEntries(snapshot)
	if len(entries) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no introspection entries are configured"
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
	for _, code := range []string{d.reportedCode("introspection_timers", ""), d.reportedCode("introspection_retire", ""), d.reportedCode("host_watch", "")} {
		if code != "" && check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckWarn, code
		}
	}
	if d.o.DryRun {
		check.Detail = "dry run: no sweep or triage timers are armed"
	}
	for _, e := range entries {
		prefix := string(e.Scope) + ": "
		var parts []string
		for _, run := range is.Runs {
			s := e.Config.Sweep
			if run == is.WorkTriage {
				s = e.Config.Triage
			}
			switch t, ok := timers[is.TimerID(e.Scope, run)]; {
			case s == nil:
				parts = append(parts, run+" by hand")
			case ok:
				parts = append(parts, run+" next "+t.FireAt.UTC().Format(time.RFC3339))
			default:
				parts = append(parts, run+" timer pending")
			}
		}
		if e.Config.HostWatch != nil {
			over := d.introspection.watcher.Over()
			switch {
			case len(over) > 0:
				parts = append(parts, "host over: "+strings.Join(over, ", "))
				if check.Status == runtime.CheckOK {
					check.Status, check.Code = runtime.CheckWarn, "host_over_threshold"
				}
			default:
				parts = append(parts, "host watch on")
			}
		}
		if e.Config.Telemetry {
			parts = append(parts, "telemetry on")
		}
		parts = append(parts, "notes "+strings.Join(e.Config.NoteNamespaces(), ","))
		if !e.Enabled {
			parts = append(parts, "scope disabled")
		}
		check.Items = append(check.Items, prefix+strings.Join(parts, "; "))
	}
	sort.Strings(check.Items)
	return check
}
