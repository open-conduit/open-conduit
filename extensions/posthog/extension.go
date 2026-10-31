package posthog

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
)

// Extension is the in-process PostHog extension (issue #66). An enabled
// scope's `posthog` entry sends that scope's agent_turn events (one per
// finished execution, from the lifecycle pipeline) and the host's
// telemetry events through its connection. Each applied generation
// replaces the entries without a restart; sinks are cached per scope and
// connection config and resolve the project key lazily, so a rotated
// reference takes effect at once. Failures are counted by the pipeline and
// reported as a doctor code; they never touch the runtime.
type Extension struct {
	host   inproc.Host
	fanout *Fanout

	mu      sync.Mutex
	entries []entry
	dryRun  bool
	sinks   map[string]*Sink // by scope + connection id + config
	failing string           // the last capture failure's code
}

type entry struct {
	scope, name string
	settings    Settings
	sink        *Sink
	err         string
}

// New returns the extension, unstarted.
func New() *Extension {
	x := &Extension{sinks: map[string]*Sink{}}
	x.fanout = &Fanout{Targets: x.targets, Diagnostic: x.diagnostic}
	return x
}

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Checker    = (*Extension)(nil)
	_ inproc.Exporting  = (*Extension)(nil)
	_ inproc.Sink       = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

// Types are the connection and the (non-ingress) entry type.
func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateConnection, Type: Type, Validate: func(raw json.RawMessage, _ []string) error { return ValidateConnection(raw) }},
		{Kind: protocol.ValidateExtension, Type: Type, Validate: ValidateExtension},
	}
}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.mu.Lock()
	x.host = h
	x.mu.Unlock()
	return nil
}

// Configure replaces the entries with the generation's, with their sinks.
func (x *Extension) Configure(_ context.Context, p protocol.ConfigureParams) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.dryRun = p.DryRun
	x.entries = nil
	for _, c := range p.Entries {
		if c.Type != Type {
			continue
		}
		st, err := ParseExtension(c.Config)
		if err != nil {
			continue
		}
		e := entry{scope: c.ScopeID, name: c.Name, settings: st}
		conn := c.Connection
		if conn == nil || conn.Type != Type {
			e.err = "posthog_connection_type"
			x.entries = append(x.entries, e)
			continue
		}
		cfg, err := ParseConnection(conn.Config)
		if err != nil {
			e.err = "posthog_connection_invalid"
			x.entries = append(x.entries, e)
			continue
		}
		key := c.ScopeID + "\x00" + conn.ID + "\x00" + string(conn.Config)
		sink := x.sinks[key]
		if sink == nil {
			scope, id, secrets := domain.ID(c.ScopeID), conn.ID, x.host.Secrets
			sink = NewSink(cfg, func(ctx context.Context) (string, error) {
				if secrets == nil {
					return "", errors.New("no secrets")
				}
				v, err := secrets(ctx, scope, id, []string{SecretToken})
				if err != nil {
					return "", err
				}
				return v[SecretToken], nil
			}, nil)
			x.sinks[key] = sink
		}
		e.sink = sink
		x.entries = append(x.entries, e)
	}
}

func (x *Extension) snapshot() ([]entry, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]entry(nil), x.entries...), x.dryRun
}

// targets are the entries the exporter and Capture send to. A daemon dry
// run sends nothing.
func (x *Extension) targets() []Target {
	entries, dry := x.snapshot()
	if dry {
		return nil
	}
	var out []Target
	for _, e := range entries {
		if e.sink != nil {
			out = append(out, Target{Scope: e.scope, Settings: e.settings, Sink: e.sink})
		}
	}
	return out
}

func (x *Extension) diagnostic(code string) {
	x.mu.Lock()
	x.failing = code
	report := x.host.Report
	x.mu.Unlock()
	if report != nil {
		report(code)
	}
}

func (x *Extension) Exporter() observability.Exporter {
	return &Exporter{Targets: x.targets, Diagnostic: x.diagnostic}
}

// Capture forwards a telemetry event to every entry that takes them.
func (x *Extension) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	return x.fanout.Capture(ctx, scope, name, at, props)
}

func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Run: x.check}, {Deep: true, Run: x.authCheck}}
}

// check reports the configured entries without service calls.
func (x *Extension) check(context.Context, protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "posthog", Status: inproc.CheckOK, Code: "ok"}
	entries, dry := x.snapshot()
	if len(entries) == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", "no PostHog entries are configured"
		return check
	}
	for _, e := range entries {
		item := e.scope + "/" + e.name + ": "
		if e.err != "" {
			check.Status, check.Code = inproc.CheckFail, e.err
			check.Items = append(check.Items, item+e.err)
			continue
		}
		var parts []string
		if e.settings.Turns {
			parts = append(parts, "agent_turn events")
		}
		if e.settings.TelemetryEvents {
			parts = append(parts, "host telemetry events")
		}
		if dry {
			parts = append(parts, "dry run (nothing sent)")
		}
		what := "nothing selected"
		if len(parts) > 0 {
			what = strings.Join(parts, ", ")
		}
		check.Items = append(check.Items, item+what)
		for _, old := range e.settings.OldNames {
			if check.Status == inproc.CheckOK {
				check.Status, check.Code = inproc.CheckWarn, "renamed_setting"
			}
			check.Items = append(check.Items, item+"`"+old+"` is now `"+Renamed[old]+"`; the old name is accepted for one release (open-conduit migrate upgrade renames it)")
		}
	}
	x.mu.Lock()
	failing := x.failing
	x.mu.Unlock()
	if failing != "" && check.Status != inproc.CheckFail {
		check.Status, check.Code = inproc.CheckWarn, "capture_failing"
		check.Items = append(check.Items, "last capture failed: "+failing)
	}
	sort.Strings(check.Items)
	return check
}

// authCheck (check_secrets only) resolves each entry's key privately and
// checks its form (PostHog cannot be asked about a key without sending an
// event, as in Open Supervisor).
func (x *Extension) authCheck(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "posthog_auth", Status: inproc.CheckOK, Code: "verified"}
	entries, _ := x.snapshot()
	if len(entries) == 0 {
		check.Status, check.Code = inproc.CheckInfo, "not_configured"
		return check
	}
	for _, e := range entries {
		item := e.scope + "/" + e.name + ": "
		if e.sink == nil {
			check.Status, check.Code = inproc.CheckFail, e.err
			check.Items = append(check.Items, item+e.err)
			continue
		}
		where, err := e.sink.Check(ctx)
		if err != nil {
			code := "credential_unavailable"
			if errors.Is(err, ErrKey) {
				code = "invalid_key"
			}
			check.Status, check.Code = inproc.CheckFail, code
			check.Items = append(check.Items, item+code)
			continue
		}
		check.Items = append(check.Items, item+where)
	}
	sort.Strings(check.Items)
	return check
}
