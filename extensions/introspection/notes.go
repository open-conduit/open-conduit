package introspection

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Agents' notes (#67 item 4). An agent logs each correction it is given and
// each problem it works around with OpCorrection and OpProblem. The notes
// are the extension's own records in NotesNamespace, keyed
// <kind>/<time>/<digest>, and carry the logging Work's correlation key
// rather than any source's issue number, so a GitHub, tracker or Slack Work
// logs them the same way. The daily sweep folds the corrections into the
// project's rules and the triage acts on the problems.

// NotesNamespace holds the notes the operations log. Handled-note records
// share it under handled/, so the two never collide.
const NotesNamespace = HandledNamespace

// Note areas: a correction's area and a problem's kind.
var (
	CorrectionAreas = []string{"writing", "design", "data", "code", "workflow"}
	ProblemKinds    = []string{"env", "docs", "tooling", "service", "other"}
)

// correctionInput is introspection.correction's input. Its fields are in
// key order, so its encoding, a call's identity, is the sorted object the
// input was.
type correctionInput struct {
	Area string `json:"area,omitempty" doc:"what the correction is about: writing, design, data, code or workflow; default workflow"`
	Body string `json:"body" doc:"the theme in one sentence, a blank line, then the rule a future agent should follow; at most 8192 bytes"`
}

// problemInput is introspection.problem's input, its fields in key order
// as correctionInput's are.
type problemInput struct {
	Body string `json:"body" doc:"what you tried in one line, then the exact error, your workaround and where the fix belongs; at most 8192 bytes"`
	Kind string `json:"kind" doc:"what went wrong: env (a missing variable, tool or credential), docs (a wrong or stale skill or rule), tooling (a toolbelt operation failed or is missing), service (the tracker, code host or deploy service misbehaving) or other"`
}

func (t *Tools) correction(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var r correctionInput
	if err := t.decodeNote(ctx, a, in, &r, "area"); err != nil {
		return domain.Payload{}, err
	}
	canonical, _ := json.Marshal(r)
	if r.Area == "" {
		r.Area = "workflow"
	}
	return t.note(ctx, a, NoteCorrection, r.Area, r.Body, canonical, CorrectionAreas, "area")
}

func (t *Tools) problem(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var r problemInput
	if err := t.decodeNote(ctx, a, in, &r, "kind"); err != nil {
		return domain.Payload{}, err
	}
	canonical, _ := json.Marshal(r)
	return t.note(ctx, a, NoteProblem, r.Kind, r.Body, canonical, ProblemKinds, "kind")
}

// decodeNote checks the scope logs notes and decodes a note's input.
func (t *Tools) decodeNote(ctx context.Context, a extension.Authority, in domain.Payload, out any, field string) error {
	if err := t.enabled(ctx, a); err != nil {
		return err
	}
	if err := strict(in.Data, out); err != nil {
		return refuse("invalid_input", "the input is malformed or has unknown fields (%s, body)", field)
	}
	return nil
}

// note logs one note. One note per request: the request ID when the caller
// supplied one, else the input, so a retried call finds the note it wrote.
func (t *Tools) note(ctx context.Context, a extension.Authority, kind, area, body string, canonical []byte, allowed []string, field string) (domain.Payload, error) {
	body = strings.TrimSpace(body)
	if !slices.Contains(allowed, area) {
		return domain.Payload{}, refuse("invalid_"+field, "%s must be one of %s", field, strings.Join(allowed, ", "))
	}
	if body == "" || len(body) > 8<<10 || !utf8.ValidString(body) {
		return domain.Payload{}, refuse("invalid_body", "the body must be 1 to 8192 bytes")
	}
	seed := extension.CallRequestID(ctx)
	if seed == "" {
		seed = "input:" + string(canonical)
	}
	suffix := "/" + digest(string(a.ExecutionID), kind, seed)[:12]
	at := t.o.Now().UTC()
	key := kind + "/" + at.Format("20060102T150405.000Z") + suffix
	err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		rows, err := tx.List(NotesNamespace, kind+"/")
		if err != nil {
			return err
		}
		for _, r := range rows {
			if strings.HasSuffix(r.Key, suffix) {
				key = r.Key
				return nil
			}
		}
		n := Note{Kind: kind, Area: area, Scope: string(a.ScopeID), Work: string(a.WorkID), Execution: string(a.ExecutionID), Body: body, At: at}
		n.Origin = noteOrigin(tx, a)
		if w, err := tx.Work(a.WorkID); err == nil && len(w.CorrelationKeys) > 0 {
			k := w.CorrelationKeys[0]
			n.Key = &k
		}
		value, err := json.Marshal(n)
		if err != nil {
			return err
		}
		return tx.Put(NotesNamespace, key, value)
	})
	if err != nil {
		return domain.Payload{}, err
	}
	return output(map[string]any{"logged": key})
}

// noteOrigin is where a note came from: the logging execution's trigger
// Event and the Work's own account of itself (its source's ref, link and
// requester, from the Work's display). It is best effort: what the store
// doesn't have stays empty.
func noteOrigin(tx storage.Tx, a extension.Authority) *Origin {
	o := Origin{Trigger: string(a.TriggerID)}
	if a.TriggerID != "" {
		if e, err := tx.Event(a.TriggerID); err == nil {
			o.TriggerType, o.TriggerSource = e.Event.Type, e.Event.Source
		}
	}
	if w, err := tx.Work(a.WorkID); err == nil && w.Display != nil {
		o.Title, o.Ref, o.Requester = w.Display.Title, w.Display.Ref, w.Display.Requester
		if len(w.Display.Links) > 0 {
			o.Link = w.Display.Links[0].URL
		}
	}
	if o == (Origin{}) {
		return nil
	}
	return &o
}

// WorkNotes counts the notes of each kind a Work's agents logged.
func WorkNotes(tx storage.Tx, work domain.ID) (map[string]int, error) {
	counts := map[string]int{}
	for _, kind := range []string{NoteCorrection, NoteProblem} {
		rows, err := tx.List(NotesNamespace, kind+"/")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			var n Note
			if json.Unmarshal(r.Value, &n) == nil && n.Work == string(work) {
				counts[kind]++
			}
		}
	}
	return counts, nil
}
