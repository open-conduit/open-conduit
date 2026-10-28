package human

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Line is one annotation of the asking Work.
type Line struct{ Name, Value string }

// shown is the Work's latest ask of the entry while it is open, and
// whether a person can answer it now (live) or only once the Work parks.
// ok is false when there is nothing to show.
func shown(ctx context.Context, store storage.Store, entry Entry, w domain.Work, now time.Time) (rec Ask, live, ok bool) {
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var err error
		if rec, err = latestAsk(tx, w.ID); err != nil {
			return err
		}
		live, err = outstanding(tx, rec, now)
		return err
	})
	if err != nil || rec.Provider != entry.Name || rec.State != StateOpen || rec.expired(now) {
		return Ask{}, false, false
	}
	// Asked in the running attempt: answerable once the Work parks.
	if !live && w.State.Terminal() {
		return Ask{}, false, false
	}
	return rec, live, true
}

// Annotate names the Work's open question while it is open, so lists and
// the CLI show it; the question panel (Panels) has the rest. ok is false
// when the Work has no open ask of the entry.
func Annotate(ctx context.Context, store storage.Store, entry Entry, w domain.Work, now time.Time) ([]Line, bool) {
	rec, _, ok := shown(ctx, store, entry, w, now)
	if !ok {
		return nil, false
	}
	return []Line{{"question", rec.Question}}, true
}

// PanelQuestion is the ID of the question panel.
const PanelQuestion = "question"

// Panel is the Work's open question as a panel: the question, its context
// and options, who it is for, its status and deadline, and the answer
// action while it can be answered. ok is false when the Work has no open
// ask of the entry.
func Panel(ctx context.Context, store storage.Store, entry Entry, w domain.Work, now time.Time) (extension.Panel, bool) {
	rec, live, ok := shown(ctx, store, entry, w, now)
	if !ok {
		return extension.Panel{}, false
	}
	status := "waiting for an answer"
	if !live {
		status = "asked; answerable once the agent parks"
	}
	sections := []extension.PanelSection{{Text: clean(rec.Question)}}
	if rec.Context != "" {
		sections = append(sections, extension.PanelSection{Title: "Context", Text: clean(rec.Context)})
	}
	if len(rec.Options) > 0 {
		t := &extension.PanelTable{Columns: []string{"#", "Option"}}
		for i, o := range rec.Options {
			t.Rows = append(t.Rows, []string{strconv.Itoa(i + 1), clean(o)})
		}
		sections = append(sections, extension.PanelSection{Title: "Options", Table: t})
	}
	about := extension.PanelSection{Fields: []extension.PanelField{{Name: "for", Value: clean(strings.Join(rec.Responders, ", "))}, {Name: "status", Value: status}}}
	if !rec.Deadline.IsZero() {
		about.Fields = append(about.Fields, extension.PanelField{Name: "answer by", Value: rec.Deadline.Format(time.RFC3339)})
	}
	if live {
		about.Actions = []string{ActionAnswer}
	}
	sections = append(sections, about)
	return extension.Panel{ID: PanelQuestion, Title: "The agent asks", Sections: sections}, true
}

// clean drops control characters a panel may not carry (all but newline
// and tab), which an agent's question could hold.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// RenderEvent renders the alias the asker wakes on: the question and the
// answer that woke it. The answer Event itself is on the ask key and reaches
// no Work, so a later answer never reaches the agent. answer looks up the
// alias's causation; without it only the fact of an answer is shown.
func RenderEvent(e domain.Event, answer func(domain.ID) (domain.Event, bool)) (string, bool) {
	if e.Type != TypeAnswered {
		return "", false
	}
	if answer != nil {
		if src, ok := answer(e.CausationID); ok {
			var p AnswerPayload
			if src.Type == TypeAnswer && src.Data.Schema == SchemaAnswer && json.Unmarshal(src.Data.Data, &p) == nil {
				return "### A person answered your question\nQuestion: " + p.Question + "\nAnswer: " + p.Answer, true
			}
		}
	}
	return "### A person answered your question\nThe answer could not be read; ask again if you need it.", true
}

// Lookup is RenderEvent's answer function over the store.
func Lookup(ctx context.Context, store storage.Store, now func() time.Time) func(domain.ID) (domain.Event, bool) {
	return func(id domain.ID) (domain.Event, bool) {
		var a domain.AcceptedEvent
		err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
			var e error
			a, e = tx.Event(id)
			return e
		})
		return a.Event, err == nil
	}
}
