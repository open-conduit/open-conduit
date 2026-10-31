package introspection

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Operation names. A binding grants each one explicitly; registering them
// grants nothing.
const (
	// OpReport reports a defect: a durable `introspection.issue.reported`
	// Event derived from the caller's trigger, plus its index record.
	OpReport = "introspection.report-issue"
	// OpList lists the calling scope's reports (for triage).
	OpList = "introspection.reports"
	// OpResolve records how a report was handled (for triage and fixes).
	OpResolve = "introspection.report.resolve"
	// OpEvidence aggregates a sweep's or triage's evidence: the scope's open
	// reports and unhandled notes, grouped, with the run's settings.
	OpEvidence = "introspection.evidence"
	// OpNotesResolve records notes a sweep or triage handled, so the next
	// run's evidence leaves them out.
	OpNotesResolve = "introspection.notes.resolve"
	// OpFix requests fix Work for reports and notes: a durable
	// `introspection.fix.requested` Event citing the earliest report as its
	// cause.
	OpFix = "introspection.fix"
	// OpCorrection and OpProblem log an agent's note: a correction it was
	// given, or a problem it worked around (inputs to sweep and triage).
	OpCorrection = "introspection.correction"
	OpProblem    = "introspection.problem"
)

// Operations lists every operation (for binding operation lists).
func Operations() []string {
	return []string{OpReport, OpList, OpResolve, OpEvidence, OpNotesResolve, OpFix, OpCorrection, OpProblem}
}

// Report states. Fixing is derived: an open report a requested fix names.
const (
	StateOpen     = "open"
	StateFixing   = "fixing"
	StateResolved = "resolved"
)

// Report is one report's index record (Namespace, key
// report/<scope>/<acceptance sequence>/<id>) and the report Event's payload. Reporter
// fields come from validated toolbelt authority, never from the caller.
type Report struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// The reporting execution, its Work and the trigger the report derives
	// from (its causal parent).
	Work      string `json:"work"`
	Execution string `json:"execution"`
	Trigger   string `json:"trigger"`
	// Event, Root and Depth place the report on its causal chain; Key is
	// the correlation its processing Work owns.
	Event      string                `json:"event,omitempty"`
	Root       string                `json:"root,omitempty"`
	Depth      int                   `json:"depth,omitempty"`
	Key        domain.CorrelationKey `json:"key"`
	At         time.Time             `json:"at"`
	State      string                `json:"state,omitempty"`
	Resolution *Resolution           `json:"resolution,omitempty"`
	// Origin names the extension adapter that made an extension-originated
	// report (host-watch); empty for a report an execution made.
	Origin string `json:"origin,omitempty"`
	// Fix is the requested fix naming this report (derived on read).
	Fix string `json:"fix,omitempty"`
}

// Resolution records how triage handled a report.
type Resolution struct {
	Summary   string    `json:"summary"`
	Link      string    `json:"link,omitempty"`
	Work      string    `json:"work"`
	Execution string    `json:"execution"`
	At        time.Time `json:"at"`
}

// ToolsOptions configure Tools.
type ToolsOptions struct {
	Store storage.Store
	Now   func() time.Time
}

// Tools implements the operations.
type Tools struct{ o ToolsOptions }

// NewTools validates options.
func NewTools(o ToolsOptions) (*Tools, error) {
	if o.Store == nil {
		return nil, storage.ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Tools{o: o}, nil
}

// Registration declares the operations and the report family through the
// public Registrar.
func (t *Tools) Registration() extension.Registration {
	return extension.Registration{Version: extension.ContractVersion, Name: Type, EventTypes: []string{TypeReported, TypeFixRequested},
		Operations: []extension.Operation{
			{Name: OpReport, Emit: t.report, Guidance: guidance("report-issue")},
			{Name: OpList, Handler: t.list},
			{Name: OpResolve, Handler: t.resolve},
			{Name: OpEvidence, Handler: t.evidence},
			{Name: OpNotesResolve, Handler: t.notesResolve},
			{Name: OpFix, Emit: t.fix},
			{Name: OpCorrection, Handler: t.correction, Guidance: guidance("correction")},
			{Name: OpProblem, Handler: t.problem, Guidance: guidance("problem")},
		}}
}

// guidanceFS holds the agent guidance of the operations agents call while
// they work, served by the toolbelt to executions granted them.
//
//go:embed guidance/*.md
var guidanceFS embed.FS

func guidance(name string) string {
	b, err := guidanceFS.ReadFile("guidance/" + name + ".md")
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(b))
}

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// enabled refuses a scope without an `introspection` entry: registration is
// global per generation, enablement is per scope.
func (t *Tools) enabled(ctx context.Context, a extension.Authority) error {
	_, err := t.entry(ctx, a)
	return err
}

// entry is the calling scope's effective entry config.
func (t *Tools) entry(ctx context.Context, a extension.Authority) (Config, error) {
	var scope domain.Scope
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var err error
		scope, err = tx.Scope(a.ScopeID)
		return err
	}); err != nil {
		return Config{}, extension.ErrUnauthorized
	}
	c, ok := scopeEntry(scope)
	if !ok {
		return Config{}, refuse("not_enabled", "introspection is not enabled for this scope")
	}
	return c, nil
}

type reportInput struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (t *Tools) report(ctx context.Context, a extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
	if err := t.enabled(ctx, a); err != nil {
		return extension.EmitPlan{}, err
	}
	var r reportInput
	if err := strict(in.Data, &r); err != nil {
		return extension.EmitPlan{}, refuse("invalid_input", "the input is malformed or has unknown fields (kind, title, body)")
	}
	r.Title, r.Body = strings.TrimSpace(r.Title), strings.TrimSpace(r.Body)
	if !slices.Contains(Kinds, r.Kind) {
		return extension.EmitPlan{}, refuse("invalid_kind", "kind must be one of %s", strings.Join(Kinds, ", "))
	}
	if r.Title == "" || len(r.Title) > 200 || strings.ContainsAny(r.Title, "\r\n") || !utf8.ValidString(r.Title) {
		return extension.EmitPlan{}, refuse("invalid_title", "the title must be one line of 1 to 200 bytes")
	}
	if r.Body == "" || len(r.Body) > 8<<10 || !utf8.ValidString(r.Body) {
		return extension.EmitPlan{}, refuse("invalid_body", "the body must be 1 to 8192 bytes")
	}
	// One report per request: the request ID when the caller supplied one,
	// else the input, so a retried call never reports twice.
	seed := extension.CallRequestID(ctx)
	if seed == "" {
		canonical, _ := json.Marshal(r)
		seed = "input:" + string(canonical)
	}
	delivery := "report/" + digest(seed)[:32]
	id := digest(string(a.ExecutionID), delivery)[:20]
	key := ReportKey(a.ScopeID, id)
	report := Report{ID: id, Scope: string(a.ScopeID), Kind: r.Kind, Title: r.Title, Body: r.Body,
		Work: string(a.WorkID), Execution: string(a.ExecutionID), Trigger: string(a.TriggerID), Key: key}
	data, err := json.Marshal(report)
	if err != nil {
		return extension.EmitPlan{}, err
	}
	return extension.EmitPlan{
		Event: domain.Event{Type: TypeReported, DeliveryID: delivery, CorrelationKey: &key, Data: domain.Payload{Schema: SchemaReport, Data: data}},
		Commit: func(e extension.Emitted) (extension.EmitCommit, error) {
			rec := report
			rec.Trigger = string(e.Event.Event.CausationID)
			rec.Event, rec.Root, rec.Depth = string(e.Event.Event.ID), string(e.Root), e.Depth
			rec.At, rec.State = e.Event.Event.ReceivedAt.UTC(), StateOpen
			value, err := json.Marshal(rec)
			if err != nil {
				return extension.EmitCommit{}, err
			}
			out, _ := json.Marshal(map[string]any{"reported": id, "event": rec.Event, "root": rec.Root, "depth": rec.Depth, "duplicate": !e.Added})
			return extension.EmitCommit{
				Records: []extension.Record{{Namespace: Namespace, Key: recordKey(a.ScopeID, e.Event.Sequence, id), Value: value}},
				Output:  domain.Payload{Schema: "introspection.tool.v1", Data: out},
			}, nil
		},
	}, nil
}

// recordKey orders a scope's records by acceptance.
func recordKey(scope domain.ID, sequence uint64, id string) string {
	return fmt.Sprintf("report/%s/%020d/%s", scope, sequence, id)
}

// Reports lists a scope's index records, oldest first (for tests and the
// operator; agents use OpList).
func Reports(tx storage.Tx, scope domain.ID) ([]Report, error) {
	rows, err := tx.List(Namespace, "report/"+string(scope)+"/")
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	fixes, err := Fixes(tx, scope)
	if err != nil {
		return nil, err
	}
	fixing := map[string]string{}
	for _, f := range fixes {
		for _, id := range f.Reports {
			fixing[id] = f.ID
		}
	}
	out := make([]Report, 0, len(rows))
	for _, row := range rows {
		var r Report
		if json.Unmarshal(row.Value, &r) == nil {
			if f, ok := fixing[r.ID]; ok {
				r.Fix = f
				if r.State == StateOpen {
					r.State = StateFixing
				}
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func output(v any) (domain.Payload, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return domain.Payload{}, err
	}
	return domain.Payload{Schema: "introspection.tool.v1", Data: data}, nil
}

func (t *Tools) list(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	if err := t.enabled(ctx, a); err != nil {
		return domain.Payload{}, err
	}
	var q struct {
		State string `json:"state"`
	}
	if err := strict(in.Data, &q); err != nil {
		return domain.Payload{}, refuse("invalid_input", "the input is malformed or has unknown fields (state)")
	}
	if q.State == "" {
		q.State = StateOpen
	}
	if q.State != StateOpen && q.State != StateFixing && q.State != StateResolved && q.State != "all" {
		return domain.Payload{}, refuse("invalid_state", "state must be open, fixing, resolved or all")
	}
	var all []Report
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var err error
		all, err = Reports(tx, a.ScopeID)
		return err
	}); err != nil {
		return domain.Payload{}, err
	}
	reports := []Report{}
	for _, r := range all {
		if q.State == "all" || r.State == q.State {
			reports = append(reports, r)
		}
	}
	return output(map[string]any{"reports": reports})
}

func (t *Tools) resolve(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	if err := t.enabled(ctx, a); err != nil {
		return domain.Payload{}, err
	}
	var r struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
		Link    string `json:"link"`
	}
	if err := strict(in.Data, &r); err != nil {
		return domain.Payload{}, refuse("invalid_input", "the input is malformed or has unknown fields (id, summary, link)")
	}
	r.Summary = strings.TrimSpace(r.Summary)
	if r.ID == "" || r.Summary == "" || len(r.Summary) > 2000 || len(r.Link) > 500 || !utf8.ValidString(r.Summary) {
		return domain.Payload{}, refuse("invalid_input", "id and a summary of 1 to 2000 bytes are required (link up to 500)")
	}
	var done Report
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		rows, err := tx.List(Namespace, "report/"+string(a.ScopeID)+"/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !strings.HasSuffix(row.Key, "/"+r.ID) {
				continue
			}
			if err := json.Unmarshal(row.Value, &done); err != nil {
				return err
			}
			if done.State == StateResolved {
				return nil
			}
			done.State = StateResolved
			done.Resolution = &Resolution{Summary: r.Summary, Link: r.Link, Work: string(a.WorkID), Execution: string(a.ExecutionID), At: t.o.Now().UTC()}
			value, err := json.Marshal(done)
			if err != nil {
				return err
			}
			return tx.Put(Namespace, row.Key, value)
		}
		return errNoReport
	})
	if errors.Is(err, errNoReport) {
		return domain.Payload{}, refuse("not_found", "this scope has no report %q", r.ID)
	}
	if err != nil {
		return domain.Payload{}, err
	}
	return output(map[string]any{"resolved": done.ID, "resolution": done.Resolution})
}

var errNoReport = errors.New("no such report")
