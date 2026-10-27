package human

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Line is one annotation of the asking Work.
type Line struct{ Name, Value string }

// Annotate shows the Work's open question in the TUI while it is open:
// the question, its context and options, who it is for and its deadline.
// ok is false when the Work has no open ask of the entry.
func Annotate(ctx context.Context, store storage.Store, entry Entry, w domain.Work, now time.Time) ([]Line, bool) {
	var rec Ask
	var live bool
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var err error
		if rec, err = latestAsk(tx, w.ID); err != nil {
			return err
		}
		live, err = outstanding(tx, rec, now)
		return err
	})
	if err != nil || rec.Provider != entry.Name || rec.State != StateOpen || rec.expired(now) {
		return nil, false
	}
	status := "waiting for an answer"
	if !live {
		// Asked in the running attempt: answerable once the Work parks.
		if w.State.Terminal() {
			return nil, false
		}
		status = "asked; answerable once the agent parks"
	}
	lines := []Line{{"question", rec.Question}}
	if rec.Context != "" {
		lines = append(lines, Line{"question context", rec.Context})
	}
	if len(rec.Options) > 0 {
		lines = append(lines, Line{"options", numbered(rec.Options)})
	}
	lines = append(lines, Line{"for", strings.Join(rec.Responders, ", ")}, Line{"question status", status})
	if !rec.Deadline.IsZero() {
		lines = append(lines, Line{"answer by", rec.Deadline.Format(time.RFC3339)})
	}
	return lines, true
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
