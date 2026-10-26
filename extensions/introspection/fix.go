package introspection

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

// FixNamespace is the coordination KV namespace of requested fixes.
const FixNamespace = "introspection.fixes"

// SchemaFix is the fix Event payload and index record schema.
const SchemaFix = "introspection.fix.v1"

// Fix is one requested fix (FixNamespace, key fix/<scope>/<acceptance
// sequence>/<id>) and the fix Event's payload. The requester fields come from
// validated toolbelt authority.
type Fix struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// Reports and Notes are what the fix addresses; ReportEvents are the
	// reports' Events, the first of which is the fix Event's cause.
	Reports      []string `json:"reports,omitempty"`
	ReportEvents []string `json:"report_events,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	// The requesting (triage) execution, its Work and its trigger.
	Work      string `json:"work"`
	Execution string `json:"execution"`
	Trigger   string `json:"trigger"`
	// Cause is the Event the fix derives from (the earliest report's, else
	// the requester's trigger); Event, Root and Depth place the fix Event on
	// that chain; FixWork is the Work that makes the fix.
	Cause   string                `json:"cause,omitempty"`
	Event   string                `json:"event,omitempty"`
	Root    string                `json:"root,omitempty"`
	Depth   int                   `json:"depth,omitempty"`
	Key     domain.CorrelationKey `json:"key"`
	FixWork string                `json:"fix_work"`
	At      time.Time             `json:"at"`
}

// FixWorkID is the Work that makes a fix.
func FixWorkID(scope domain.ID, id string) domain.ID {
	return domain.ID(SourceNamespace(scope) + "/fix/" + id)
}

// Fixes lists a scope's requested fixes, oldest first.
func Fixes(tx storage.Tx, scope domain.ID) ([]Fix, error) {
	rows, err := tx.List(FixNamespace, "fix/"+string(scope)+"/")
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	out := make([]Fix, 0, len(rows))
	for _, row := range rows {
		var f Fix
		if json.Unmarshal(row.Value, &f) == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

type fixInput struct {
	Reports []string `json:"reports"`
	Notes   []string `json:"notes"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
}

// fix requests fix Work. The Event is addressed to the fix's own key, so
// the `introspection` route (work: fix) gives it Work of its own on the
// user's fix binding, and it cites the earliest named report's Event as its
// cause: the fix and everything its Work does trace through causation to the
// report and the report's origin, while the trigger of the requesting
// (triage) execution still floors its chain position and budgets.
func (t *Tools) fix(ctx context.Context, a extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
	cfg, err := t.entry(ctx, a)
	if err != nil {
		return extension.EmitPlan{}, err
	}
	var r fixInput
	if err := strict(in.Data, &r); err != nil {
		return extension.EmitPlan{}, refuse("invalid_input", "the input is malformed or has unknown fields (reports, notes, title, body)")
	}
	r.Title, r.Body = strings.TrimSpace(r.Title), strings.TrimSpace(r.Body)
	if r.Title == "" || len(r.Title) > 200 || strings.ContainsAny(r.Title, "\r\n") || !utf8.ValidString(r.Title) {
		return extension.EmitPlan{}, refuse("invalid_title", "the title must be one line of 1 to 200 bytes")
	}
	if r.Body == "" || len(r.Body) > 16<<10 || !utf8.ValidString(r.Body) {
		return extension.EmitPlan{}, refuse("invalid_body", "the body must be 1 to 16384 bytes")
	}
	if len(r.Reports)+len(r.Notes) == 0 || len(r.Reports) > 100 || len(r.Notes) > 100 {
		return extension.EmitPlan{}, refuse("invalid_input", "name 1 to 100 reports and/or notes the fix addresses")
	}
	var reports []Report
	var known map[string]bool
	err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		all, err := Reports(tx, a.ScopeID)
		if err != nil {
			return err
		}
		byID := map[string]Report{}
		for _, rep := range all {
			byID[rep.ID] = rep
		}
		for _, id := range r.Reports {
			rep, ok := byID[id]
			if !ok {
				return refuse("not_found", "this scope has no report %q", id)
			}
			if rep.State == StateResolved {
				return refuse("resolved", "report %s is already resolved", id)
			}
			reports = append(reports, rep)
		}
		known, err = noteIDs(tx, cfg, a.ScopeID, r.Notes)
		return err
	})
	if err != nil {
		return extension.EmitPlan{}, err
	}
	for _, id := range r.Notes {
		if !known[id] {
			return extension.EmitPlan{}, refuse("not_found", "this scope has no note %q", id)
		}
	}
	// Acceptance order: the earliest report is the cause.
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].At.Before(reports[j].At) })
	f := Fix{Scope: string(a.ScopeID), Title: r.Title, Body: r.Body, Notes: slices.Clone(r.Notes),
		Work: string(a.WorkID), Execution: string(a.ExecutionID), Trigger: string(a.TriggerID)}
	for _, rep := range reports {
		if slices.Contains(f.Reports, rep.ID) {
			continue
		}
		f.Reports, f.ReportEvents = append(f.Reports, rep.ID), append(f.ReportEvents, rep.Event)
	}
	seed := extension.CallRequestID(ctx)
	if seed == "" {
		canonical, _ := json.Marshal(r)
		seed = "input:" + string(canonical)
	}
	delivery := "fix/" + digest(seed)[:32]
	f.ID = digest(string(a.ExecutionID), delivery)[:20]
	f.Key, f.FixWork = FixKey(a.ScopeID, f.ID), string(FixWorkID(a.ScopeID, f.ID))
	var cause domain.ID
	if len(f.ReportEvents) > 0 && f.ReportEvents[0] != "" {
		cause = domain.ID(f.ReportEvents[0])
	}
	f.Cause = string(cause)
	data, err := json.Marshal(f)
	if err != nil {
		return extension.EmitPlan{}, err
	}
	key := f.Key
	return extension.EmitPlan{
		Cause: cause,
		Event: domain.Event{Type: TypeFixRequested, DeliveryID: delivery, CorrelationKey: &key, Data: domain.Payload{Schema: SchemaFix, Data: data}},
		Commit: func(e extension.Emitted) (extension.EmitCommit, error) {
			rec := f
			rec.Cause = string(e.Event.Event.CausationID)
			rec.Event, rec.Root, rec.Depth, rec.At = string(e.Event.Event.ID), string(e.Root), e.Depth, e.Event.Event.ReceivedAt.UTC()
			value, err := json.Marshal(rec)
			if err != nil {
				return extension.EmitCommit{}, err
			}
			out, _ := json.Marshal(map[string]any{"fix": f.ID, "work": f.FixWork, "event": rec.Event, "cause": rec.Cause, "root": rec.Root, "depth": rec.Depth, "duplicate": !e.Added})
			return extension.EmitCommit{
				Records: []extension.Record{{Namespace: FixNamespace, Key: fmt.Sprintf("fix/%s/%020d/%s", a.ScopeID, e.Event.Sequence, f.ID), Value: value}},
				Output:  domain.Payload{Schema: "introspection.tool.v1", Data: out},
			}, nil
		},
	}, nil
}
