package setup

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// Extension is the in-process `setup` extension. The entry type, the route
// and the session Work kind are its own; its operations join a generation's
// toolbelt only when an enabled scope has an entry, and each scope's entry
// is an action provider under the entry's name.
type Extension struct {
	host  inproc.Host
	tools *Tools

	mu      sync.Mutex
	entries map[domain.ID]string // entry name by enabled scope
}

// New returns the extension, unstarted.
func New() *Extension { return &Extension{} }

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Operator   = (*Extension)(nil)
	_ inproc.Router     = (*Extension)(nil)
	_ inproc.WorkKinder = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Tooled     = (*Extension)(nil)
	_ inproc.EntryGated = (*Extension)(nil)
	_ inproc.Actor      = (*Extension)(nil)
	_ inproc.Renderer   = (*Extension)(nil)
	_ inproc.Playbooker = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateExtension, Type: Type, Validate: ValidateExtension},
		{Kind: protocol.ValidateRoute, Type: Type, Validate: ValidateRoute, Binding: ValidateBinding},
	}
}

func (*Extension) Operations() []string { return Operations() }

func (*Extension) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{Type: Route{}}
}

func (*Extension) WorkKinds() []inproc.WorkKind { return []inproc.WorkKind{{Kind: WorkKind}} }

func (*Extension) EntryGated() {}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	tools, err := NewTools(Options{Store: h.Store, Accept: h.AcceptInTx, Enabled: x.enabled, Now: h.Now})
	if err != nil {
		return err
	}
	x.host, x.tools = h, tools
	return nil
}

// Configure keeps each enabled scope's first `setup` entry.
func (x *Extension) Configure(_ context.Context, p protocol.ConfigureParams) {
	entries := map[domain.ID]string{}
	for _, e := range p.Entries {
		scope := domain.ID(e.ScopeID)
		if _, seen := entries[scope]; e.Type != Type || seen || ValidateExtension(e.Config, nil) != nil {
			continue
		}
		entries[scope] = e.Name
	}
	x.mu.Lock()
	x.entries = entries
	x.mu.Unlock()
}

func (x *Extension) entry(scope domain.ID) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	name, ok := x.entries[scope]
	return name, ok
}

func (x *Extension) enabled(scope domain.ID) bool {
	_, ok := x.entry(scope)
	return ok
}

func (x *Extension) Registration() extension.Registration { return x.tools.Registration() }

func (x *Extension) Actions(scope domain.ID) map[string]extension.ActionProvider {
	name, ok := x.entry(scope)
	if !ok {
		return nil
	}
	return map[string]extension.ActionProvider{name: Actions{Store: x.host.Store, Now: x.host.Now}}
}

func (*Extension) Render(_ context.Context, e domain.Event) (string, bool) { return RenderEvent(e) }

// RenderEvent renders the operator's input for the session's agent.
func RenderEvent(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeStart:
		var s Start
		if e.Data.Schema != SchemaStart || json.Unmarshal(e.Data.Data, &s) != nil {
			return "", false
		}
		out := "### The operator started a setup session"
		if s.Model != "" {
			out += " (on " + s.Model + ")"
		}
		return out + "\nSpeak to them with `" + OpSay + "` and ask with `" + OpAsk + "`: they see nothing else you write.", true
	case TypeMessage:
		var m Message
		if e.Data.Schema != SchemaMessage || json.Unmarshal(e.Data.Data, &m) != nil || m.Role != RoleOperator {
			return "", false
		}
		return "### The operator says\n" + m.Text, true
	case TypeAnswer:
		var a Answer
		if e.Data.Schema != SchemaAnswer || json.Unmarshal(e.Data.Data, &a) != nil {
			return "", false
		}
		return "### The operator answered your question\n" + a.Answer, true
	}
	return "", false
}
