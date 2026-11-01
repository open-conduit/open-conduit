package observe

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Tools implements observe.record.
type Tools struct {
	store storage.Store
	now   func() time.Time
}

// NewTools returns the tools over store.
func NewTools(store storage.Store, now func() time.Time) (*Tools, error) {
	if store == nil {
		return nil, storage.ErrInvalid
	}
	if now == nil {
		now = time.Now
	}
	return &Tools{store: store, now: now}, nil
}

// recordGuidance tells an agent when and how to record; the toolbelt serves
// it to executions granted observe.record.
//
//go:embed guidance/record.md
var recordGuidance string

// Registration is the toolbelt registration.
func (t *Tools) Registration() extension.Registration {
	return extension.Registration{Version: extension.ContractVersion, Name: Type, EventTypes: []string{TypeRecorded},
		Operations: []extension.Operation{{Name: OpRecord, Emit: t.record, Guidance: strings.TrimSpace(recordGuidance)}}}
}

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// recordInput is observe.record's input.
type recordInput struct {
	Source   string `json:"source,omitempty"`
	Item     string `json:"item"`
	Revision string `json:"revision,omitempty"`
	Thread   string `json:"thread"`
	Text     string `json:"text"`
}

func (in recordInput) check() error {
	for _, f := range []struct {
		name, value string
		min, max    int
	}{{"item", in.Item, 1, MaxID}, {"thread", in.Thread, 1, MaxID}, {"revision", in.Revision, 0, MaxRevision}, {"text", in.Text, 0, MaxText}} {
		if len(f.value) < f.min || len(f.value) > f.max || !utf8.ValidString(f.value) {
			if f.min > 0 {
				return refuse("bad_request", "%s must be 1 to %d bytes of UTF-8", f.name, f.max)
			}
			return refuse("bad_request", "%s must be at most %d bytes of UTF-8", f.name, f.max)
		}
	}
	return nil
}

// record checks that the calling binding may report for the source and
// plans the item's Event. The source comes from the input only when the
// binding reports for several; the binding comes from the stored Execution,
// never from the caller.
func (t *Tools) record(ctx context.Context, a extension.Authority, raw domain.Payload) (extension.EmitPlan, error) {
	var in recordInput
	if err := strict(raw.Data, &in); err != nil {
		return extension.EmitPlan{}, refuse("bad_request", "give item, thread and text, and optionally revision and source")
	}
	if err := in.check(); err != nil {
		return extension.EmitPlan{}, err
	}
	var binding string
	var scope domain.Scope
	if err := t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		x, err := tx.Execution(a.ExecutionID)
		if err != nil {
			return err
		}
		binding = x.ExecutorBinding
		scope, err = tx.Scope(a.ScopeID)
		return err
	}); err != nil {
		return extension.EmitPlan{}, err
	}
	mine := ReportsFor(scope, binding)
	switch {
	case len(mine) == 0:
		return extension.EmitPlan{}, refuse("not_a_reporter", "binding %s reports for no source in this scope", binding)
	case in.Source == "" && len(mine) > 1:
		return extension.EmitPlan{}, refuse("source_required", "name the source: this binding reports for %s", strings.Join(mine, ", "))
	case in.Source == "":
		in.Source = mine[0]
	case !slices.Contains(mine, in.Source):
		return extension.EmitPlan{}, refuse("not_a_reporter", "binding %s may not report for %s", binding, in.Source)
	}
	key := Key(a.ScopeID, in.Source, in.Thread)
	data, err := json.Marshal(Item(in))
	if err != nil {
		return extension.EmitPlan{}, err
	}
	once := digest(in.Source, in.Item, in.Revision)
	return extension.EmitPlan{
		Event: domain.Event{Type: TypeRecorded, DeliveryID: "item/" + once[:32], CorrelationKey: &key, Data: domain.Payload{Schema: SchemaItem, Data: data}},
		Once:  once,
		Commit: func(m extension.Emitted) (extension.EmitCommit, error) {
			out, err := json.Marshal(map[string]any{"source": in.Source, "item": in.Item, "revision": in.Revision, "thread": in.Thread, "duplicate": !m.Added})
			return extension.EmitCommit{Output: domain.Payload{Schema: "observe.record.v1", Data: out}}, err
		},
	}, nil
}
