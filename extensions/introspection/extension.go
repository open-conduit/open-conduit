package introspection

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
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/introspection/host"
	"github.com/open-conduit/open-conduit/extensions/introspection/playbooks"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// ensureGap is the least time between timer and removal passes, besides the
// pass each applied generation forces.
const ensureGap = time.Minute

// Extension is the in-process `introspection` extension (issue #70). Every
// feature hangs off the current generation's `introspection` entries:
//
//   - the entry type, the route and its config validator, the operations
//     and Event families (on a generation's toolbelt only when an enabled
//     scope has an entry), prompt rendering and the playbooks;
//   - the standing sweep/triage Work kinds and the calendar recurrence,
//     whose timers a task reconciles with each generation (Ensure) through
//     the host's timers;
//   - Work the configuration no longer has a use for, retired through the
//     host's CancelWork (Retired);
//   - the optional host watch, a task that accepts its reports as root
//     Events of the report family, and telemetry events, a lifecycle
//     exporter whose events, like the host watch's readings, go to the
//     host's telemetry events log.
//
// Removing or disabling the entries therefore removes the features without
// any change to core.
type Extension struct {
	host     inproc.Host
	tools    *Tools
	exporter *Exporter
	watcher  HostWatcher
	// telemetry is whether an enabled entry turns telemetry on, for the
	// exporter's pipeline goroutine.
	telemetry atomic.Bool

	mu         sync.Mutex
	configured bool
	enabled    map[domain.ID]Config // enabled scopes' entries
	force      bool                 // a new generation: reconcile now
	lastEnsure time.Time
	lastRead   time.Time
	codes      map[string]string // diagnostic code by pass
}

// New returns the extension, unstarted.
func New() *Extension {
	x := &Extension{codes: map[string]string{}}
	x.exporter = &Exporter{Sink: hostSink{x}, Enabled: x.telemetry.Load}
	return x
}

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Operator   = (*Extension)(nil)
	_ inproc.Router     = (*Extension)(nil)
	_ inproc.WorkKinder = (*Extension)(nil)
	_ inproc.Recurring  = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Tooled     = (*Extension)(nil)
	_ inproc.EntryGated = (*Extension)(nil)
	_ inproc.Annotator  = (*Extension)(nil)
	_ inproc.Renderer   = (*Extension)(nil)
	_ inproc.Tasker     = (*Extension)(nil)
	_ inproc.Checker    = (*Extension)(nil)
	_ inproc.Playbooker = (*Extension)(nil)
	_ inproc.Exporting  = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateExtension, Type: Type, Validate: ValidateExtension},
		{Kind: protocol.ValidateRoute, Type: Type, Validate: func(raw json.RawMessage, _ []string) error { return ValidateRoute(raw) }},
	}
}

func (*Extension) Operations() []string { return Operations() }

func (*Extension) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{Type: Route{}}
}

// WorkKinds declares the extension's Work kinds: the daily runs are
// standing, a report or fix is ordinary Work.
func (*Extension) WorkKinds() []inproc.WorkKind {
	return []inproc.WorkKind{{Kind: WorkKind}, {Kind: WorkKindSweep, Standing: true}, {Kind: WorkKindTriage, Standing: true}, {Kind: WorkKindFix}}
}

func (*Extension) Recurrences() map[string]inproc.RecurrenceFunc {
	return map[string]inproc.RecurrenceFunc{RecurrenceKind: Recurrence}
}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	tools, err := NewTools(ToolsOptions{Store: h.Store, Now: h.Now})
	if err != nil {
		return err
	}
	x.host, x.tools = h, tools
	return nil
}

// Configure keeps each enabled scope's entry and reconciles timers and
// Work with the generation at once (startup and every committed apply), as
// the minute-paced task does between generations.
func (x *Extension) Configure(ctx context.Context, p protocol.ConfigureParams) {
	enabled := map[domain.ID]Config{}
	telemetry := false
	for _, e := range p.Entries {
		scope := domain.ID(e.ScopeID)
		if e.Type != Type {
			continue
		}
		if _, seen := enabled[scope]; seen {
			continue
		}
		c, _ := ParseConfig(e.Config)
		enabled[scope] = c
		telemetry = telemetry || c.Telemetry
	}
	x.telemetry.Store(telemetry)
	x.mu.Lock()
	x.configured, x.enabled, x.force = true, enabled, true
	x.mu.Unlock()
	if err := x.ensure(ctx); err != nil {
		x.setCode("timers", code(err))
	}
}

func (x *Extension) Registration() extension.Registration { return x.tools.Registration() }

// EntryGated: the operations join a generation only with an entry.
func (*Extension) EntryGated() {}

// Annotate counts the notes a Work's agents logged.
func (x *Extension) Annotate(ctx context.Context, w domain.Work) []inproc.Annotation {
	x.mu.Lock()
	_, ok := x.enabled[w.ScopeID]
	x.mu.Unlock()
	if !ok {
		return nil
	}
	var counts map[string]int
	_ = x.host.Store.Transact(ctx, storage.Guard{Now: x.host.Now()}, func(tx storage.Tx) error {
		var err error
		counts, err = WorkNotes(tx, w.ID)
		return err
	})
	var out []inproc.Annotation
	for _, kind := range []string{NoteCorrection, NoteProblem} {
		if counts[kind] > 0 {
			out = append(out, inproc.Annotation{Source: Type, Name: kind + "s", Value: fmt.Sprint(counts[kind])})
		}
	}
	return out
}

func (*Extension) Render(_ context.Context, e domain.Event) (string, bool) { return RenderEvent(e) }

func (*Extension) Playbooks() map[string]string { return playbooks.All() }

func (x *Extension) Exporter() observability.Exporter { return x.exporter }

// hostSink hands the exporter's events to the host's telemetry events log
// once the extension has started.
type hostSink struct{ x *Extension }

func (c hostSink) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	if c.x.host.Capture == nil {
		return nil
	}
	return c.x.host.Capture(ctx, scope, name, at, props)
}

// Tasks reconcile timers and retire Work (every minute, and at once after a
// new generation), and take the host watch's readings when due.
func (x *Extension) Tasks() []inproc.Task {
	return []inproc.Task{
		{Name: "ensure", Run: x.ensure},
		{Name: "host-watch", Run: x.hostWatch},
	}
}

// entries are every scope's entry: the enabled scopes' from configure, and
// a disabled scope's from its stored configuration, whose timers and Work
// wait for it (B05). configured maps every stored scope to whether it has
// an entry, for the removal policy.
func (x *Extension) entries(tx storage.Tx) ([]Entry, map[domain.ID]bool, error) {
	scopes, err := tx.ListScopes()
	if err != nil {
		return nil, nil, err
	}
	x.mu.Lock()
	enabled := x.enabled
	x.mu.Unlock()
	var out []Entry
	configured := map[domain.ID]bool{}
	for _, s := range scopes {
		if c, ok := enabled[s.ID]; ok {
			out = append(out, Entry{Scope: s.ID, Enabled: true, Config: c})
			configured[s.ID] = true
			continue
		}
		c, on, has := storedEntry(s)
		if on || !has {
			// Enabled without an entry in this generation (or a scope the
			// configuration no longer has): nothing to keep.
			configured[s.ID] = false
			continue
		}
		out = append(out, Entry{Scope: s.ID, Enabled: false, Config: c})
		configured[s.ID] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out, configured, nil
}

func (x *Extension) snapshot(ctx context.Context) ([]Entry, map[domain.ID]bool, error) {
	var entries []Entry
	var configured map[domain.ID]bool
	err := x.host.Store.Transact(ctx, storage.Guard{Now: x.host.Now()}, func(tx storage.Tx) error {
		var err error
		entries, configured, err = x.entries(tx)
		return err
	})
	return entries, configured, err
}

func (x *Extension) setCode(pass, code string) {
	x.mu.Lock()
	x.codes[pass] = code
	var all []string
	for _, p := range []string{"timers", "retire", "host_watch"} {
		if c := x.codes[p]; c != "" {
			all = append(all, p+":"+c)
		}
	}
	x.mu.Unlock()
	if x.host.Report != nil {
		x.host.Report(strings.Join(all, ","))
	}
}

func (x *Extension) code(pass string) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.codes[pass]
}

// ensure reconciles the sweep/triage timers with the generation and
// retires Work it no longer has a use for. A dry run arms nothing, as for
// scheduled jobs.
func (x *Extension) ensure(ctx context.Context) error {
	x.mu.Lock()
	due := x.configured && (x.force || x.host.Now().Sub(x.lastEnsure) >= ensureGap)
	if due {
		x.force, x.lastEnsure = false, x.host.Now()
	}
	x.mu.Unlock()
	if !due {
		return nil
	}
	entries, configured, err := x.snapshot(ctx)
	if err != nil {
		return err
	}
	if !x.host.DryRun {
		res, err := Ensure(ctx, x.host.Store, x.host.Now(), x.host.Timers, entries)
		c := strings.Join(res.Codes, ",")
		if err != nil {
			c = code(err)
		}
		x.setCode("timers", c)
		if res.Armed+res.Rearmed+res.Cancelled > 0 {
			x.host.Log.Info("introspection timers reconciled", "armed", res.Armed, "rearmed", res.Rearmed, "cancelled", res.Cancelled, "kept", res.Kept)
			x.nudge()
		}
	}
	x.retire(ctx, configured)
	return nil
}

// retire applies the removal policy: Work of the extension's kinds the
// configuration no longer has a use for is cancelled, with a recorded
// reason, once no attempt of it is running.
func (x *Extension) retire(ctx context.Context, configured map[domain.ID]bool) {
	var retired []domain.Work
	err := x.host.Store.Transact(ctx, storage.Guard{Now: x.host.Now()}, func(tx storage.Tx) error {
		var err error
		retired, err = Retired(tx, configured)
		return err
	})
	c := ""
	if err != nil {
		c = code(err)
	}
	for _, w := range retired {
		key := "retire/" + string(w.ID) + "/" + fmt.Sprint(w.Revision)
		if _, err := x.host.CancelWork(ctx, w.ID, w.Revision, RetireReason, key); err != nil {
			if !errors.Is(err, storage.ErrConflict) {
				c = code(err)
			}
			continue
		}
		x.host.Log.Info("introspection work retired", "work", string(w.ID), "kind", w.Kind)
	}
	x.setCode("retire", c)
	if len(retired) > 0 {
		x.nudge()
	}
}

func (x *Extension) nudge() {
	if x.host.Nudge != nil {
		x.host.Nudge()
	}
}

// hostWatch takes a reading when one is due, reports each threshold crossed
// over as an extension-originated report into every enabled scope whose
// entry watches the host, and captures the reading as telemetry when an
// entry turns telemetry on.
func (x *Extension) hostWatch(ctx context.Context) error {
	x.mu.Lock()
	var watched []Entry
	interval := time.Duration(0)
	var disks []string
	for scope, c := range x.enabled {
		if c.HostWatch == nil {
			continue
		}
		watched = append(watched, Entry{Scope: scope, Enabled: true, Config: c})
		if i := c.HostWatch.Interval(); interval == 0 || i < interval {
			interval = i
		}
		for _, p := range c.HostWatch.Disks {
			if strings.HasPrefix(p, "~/") {
				if home, err := os.UserHomeDir(); err == nil {
					p = filepath.Join(home, p[2:])
				}
			}
			disks = append(disks, p)
		}
	}
	due := len(watched) > 0 && x.host.Now().Sub(x.lastRead) >= interval
	if due {
		x.lastRead = x.host.Now()
	}
	x.mu.Unlock()
	if !due {
		return nil
	}
	sort.Slice(watched, func(i, j int) bool { return watched[i].Scope < watched[j].Scope })
	sort.Strings(disks)
	sample, err := host.Read(append([]string{x.host.StateDir}, disks...))
	if err != nil {
		x.setCode("host_watch", "unreadable")
		return nil
	}
	at := x.host.Now()
	telemetry := x.telemetry.Load()
	if telemetry {
		_ = x.exporter.Sink.Capture(ctx, "", EventHostMetrics, at, HostProps(sample))
	}
	// One watcher for the machine; the first watching entry's thresholds
	// decide (a machine has one set of readings).
	alerts := x.watcher.Update(sample, *watched[0].Config.HostWatch)
	c := ""
	for _, a := range alerts {
		x.host.Log.Info(a.String())
		if telemetry {
			props := map[string]any{"metric": a.Metric, "value": a.Value, "threshold": a.Threshold, "over": a.Over}
			if a.Path != "" {
				props["path"] = a.Path
			}
			_ = x.exporter.Sink.Capture(ctx, "", EventHostAlert, at, props)
		}
		if !a.Over {
			continue
		}
		for _, e := range watched {
			ev, rep := HostReport(e.Scope, a, at)
			if err := x.host.Store.Transact(ctx, storage.Guard{Now: at}, func(tx storage.Tx) error {
				accepted, added, err := x.host.AcceptInTx(tx, e.Scope, ev)
				if err != nil || !added {
					return err
				}
				return PutOriginInTx(tx, accepted, rep)
			}); err != nil {
				c = code(err)
			}
		}
		x.nudge()
	}
	x.setCode("host_watch", c)
	return nil
}

// Checks reports each entry's features for doctor.
func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Run: x.check}}
}

// check reports each entry's features: the sweep/triage schedules with
// their next slot, the host watch's readings over threshold, telemetry and
// the notes it reads. It makes no service calls.
func (x *Extension) check(ctx context.Context) inproc.CheckResult {
	check := inproc.CheckResult{Name: "introspection", Status: inproc.CheckOK, Code: "ok"}
	var entries []Entry
	timers := map[domain.ID]domain.Timer{}
	_ = x.host.Store.Transact(ctx, storage.Guard{Now: x.host.Now()}, func(tx storage.Tx) error {
		var err error
		if entries, _, err = x.entries(tx); err != nil {
			return err
		}
		all, err := tx.ListTimers()
		for _, t := range all {
			timers[t.ID] = t
		}
		return err
	})
	if len(entries) == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", "no introspection entries are configured"
		return check
	}
	for _, pass := range []string{"timers", "retire", "host_watch"} {
		if c := x.code(pass); c != "" && check.Status == inproc.CheckOK {
			check.Status, check.Code = inproc.CheckWarn, c
		}
	}
	if x.host.DryRun {
		check.Detail = "dry run: no sweep or triage timers are armed"
	}
	for _, e := range entries {
		prefix := string(e.Scope) + ": "
		var parts []string
		for _, run := range Runs {
			s := e.Config.Sweep
			if run == WorkTriage {
				s = e.Config.Triage
			}
			switch t, ok := timers[TimerID(e.Scope, run)]; {
			case s == nil:
				parts = append(parts, run+" by hand")
			case ok:
				parts = append(parts, run+" next "+t.FireAt.UTC().Format(time.RFC3339))
			default:
				parts = append(parts, run+" timer pending")
			}
		}
		if e.Config.HostWatch != nil {
			over := x.watcher.Over()
			switch {
			case len(over) > 0:
				parts = append(parts, "host over: "+strings.Join(over, ", "))
				if check.Status == inproc.CheckOK {
					check.Status, check.Code = inproc.CheckWarn, "host_over_threshold"
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
