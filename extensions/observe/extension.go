package observe

import (
	"context"
	"encoding/json"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// Extension is the in-process `observe` extension: the entry type, the
// route type and its handler, observe.record (on a generation's toolbelt
// only when an enabled scope has an entry) and the observe.thread Work
// kind. It reads its entries from each scope's own config, so it keeps no
// state of its own.
type Extension struct {
	tools *Tools
}

// New returns the extension, unstarted.
func New() *Extension { return &Extension{} }

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Operator   = (*Extension)(nil)
	_ inproc.Router     = (*Extension)(nil)
	_ inproc.WorkKinder = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Tooled     = (*Extension)(nil)
	_ inproc.EntryGated = (*Extension)(nil)
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

// WorkKinds declares a thread's Work as ordinary Work: it parks on a wait
// between items and ends when its agent says the thread is done.
func (*Extension) WorkKinds() []inproc.WorkKind { return []inproc.WorkKind{{Kind: WorkKind}} }

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	tools, err := NewTools(h.Store, h.Now)
	if err != nil {
		return err
	}
	x.tools = tools
	return nil
}

func (x *Extension) Registration() extension.Registration { return x.tools.Registration() }

func (*Extension) EntryGated() {}
