package human

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// syncPace is the least time between ask settlement passes.
const syncPace = 5 * time.Second

// Extension is the in-process `human` extension (issue #66). The entry type
// is its config type and human.ask joins every generation's toolbelt (it
// refuses in a scope without an entry). A scope's entry is an action
// provider under its entry name, offering "answer" on Work parked on one of
// its asks, and annotates that Work with the question while it is open. A
// task settles ask records (answered, expired, withdrawn). Nothing here
// projects to a Work's source.
type Extension struct {
	host  inproc.Host
	tools *Tools

	mu      sync.Mutex
	entries map[domain.ID]Entry // the first entry of each enabled scope
}

// New returns the extension, unstarted.
func New() *Extension { return &Extension{} }

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Operator   = (*Extension)(nil)
	_ inproc.Declarer   = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Tooled     = (*Extension)(nil)
	_ inproc.Actor      = (*Extension)(nil)
	_ inproc.Annotator  = (*Extension)(nil)
	_ inproc.Renderer   = (*Extension)(nil)
	_ inproc.Tasker     = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{{Kind: protocol.ValidateExtension, Type: Type, Validate: ValidateExtension, Fields: []protocol.FieldDecl{
		{Name: "responders", Help: "who may answer, each kind:id or a bare kind", Default: `["operator:local"]`},
		{Name: "surfaces", Help: "where the question is shown", Default: `["tui"]`},
		{Name: "gates", Help: `[{"operations": ["github.pr.create"], "responders": ["operator:local"], "timeout": "24h"}]: operations that need a person's approval before they run`},
	}}}
}

func (*Extension) Operations() []string { return Operations() }

func (*Extension) Declarations() []extension.Operation { return Declared() }

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	tools, err := NewTools(Options{Store: h.Store, Entry: x.entry, Now: h.Now})
	if err != nil {
		return err
	}
	x.host, x.tools = h, tools
	return nil
}

// Configure keeps each enabled scope's first `human` entry.
func (x *Extension) Configure(_ context.Context, p protocol.ConfigureParams) {
	entries := map[domain.ID]Entry{}
	for _, e := range p.Entries {
		scope := domain.ID(e.ScopeID)
		if e.Type != Type {
			continue
		}
		if _, seen := entries[scope]; seen {
			continue
		}
		c, err := ParseConfig(e.Config)
		if err != nil {
			entries[scope] = Entry{}
			continue
		}
		entries[scope] = Entry{Name: e.Name, Config: c}
	}
	x.mu.Lock()
	x.entries = entries
	x.mu.Unlock()
}

// entry is the scope's `human` entry in the current generation, when the
// scope is enabled and has a valid one.
func (x *Extension) entry(scope domain.ID) (Entry, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.entries[scope]
	return e, ok && e.Name != ""
}

func (x *Extension) Registration() extension.Registration { return x.tools.Registration() }

func (x *Extension) Actions(scope domain.ID) map[string]extension.ActionProvider {
	entry, ok := x.entry(scope)
	if !ok {
		return nil
	}
	return map[string]extension.ActionProvider{entry.Name: Actions{Store: x.host.Store, Entry: entry, Now: x.host.Now}}
}

// Annotate shows the Work's open question.
func (x *Extension) Annotate(ctx context.Context, w domain.Work) []inproc.Annotation {
	entry, ok := x.entry(w.ScopeID)
	if !ok {
		return nil
	}
	lines, ok := Annotate(ctx, x.host.Store, entry, w, x.host.Now())
	if !ok {
		return nil
	}
	out := make([]inproc.Annotation, 0, len(lines))
	for _, l := range lines {
		out = append(out, inproc.Annotation{Source: entry.Name, Name: l.Name, Value: l.Value})
	}
	return out
}

func (x *Extension) Render(ctx context.Context, e domain.Event) (string, bool) {
	return RenderEvent(e, Lookup(ctx, x.host.Store, x.host.Now))
}

// Tasks settles ask records.
func (x *Extension) Tasks() []inproc.Task {
	return []inproc.Task{{Name: "asks", Pace: syncPace, Run: func(ctx context.Context) error {
		res, err := Sync(ctx, x.host.Store, x.host.Now())
		if res.Late > 0 && x.host.Log != nil {
			x.host.Log.Info("late answers recorded", "late", res.Late)
		}
		for _, a := range res.Escalated {
			x.escalated(ctx, a)
		}
		if err != nil || x.host.SettleHold == nil {
			return err
		}
		n, err := SettleStarts(ctx, x.host.Store, x.host.Now(), x.host.SettleHold)
		if n > 0 && x.host.Log != nil {
			x.host.Log.Info("start approvals settled", "settled", n)
		}
		return err
	}}}
}

// EventGateEscalated is the telemetry event an approval request records
// when nobody answered by its gate's timeout and it escalated.
const EventGateEscalated = "human_gate_escalated"

// escalated leaves a trail of an approval request's escalation: telemetry
// linked to its Work, and a log line.
func (x *Extension) escalated(ctx context.Context, a Ask) {
	if x.host.Log != nil {
		x.host.Log.Info("approval request escalated", "work", a.Work, "operation", a.Gate, "to", strings.Join(a.EscalateTo, ","))
	}
	if x.host.Capture != nil {
		_ = x.host.Capture(ctx, string(a.Scope), EventGateEscalated, a.EscalatedAt, map[string]any{
			"work_id": string(a.Work), "execution_id": string(a.Execution), "ask_id": a.ID, "operation": a.Gate,
			"escalated_to": a.EscalateTo, "deadline": a.Deadline.UTC().Format(time.RFC3339)})
	}
}
