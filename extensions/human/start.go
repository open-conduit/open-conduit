package human

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// StartGate is the extension's start gate (extension.StartGate). Work whose
// kind, or the binding its first attempt runs on, one of the scope's
// `human` gates names needs a person's approval before it first starts: the
// first time it would start, an approval request is recorded on the Work
// and the Work is held. Nothing waits on the request: settling it
// (SettleStarts, from the asks task) lets the Work start once when it is
// approved, and cancels it when it is refused or nobody answers in time.
// Work the scope's gates don't name starts, and so does all Work in a scope
// without a `human` entry.
func (t *Tools) StartGate(ctx context.Context, c extension.StartCall) (*extension.StartHold, error) {
	entry, ok := t.o.Entry(c.Work.ScopeID)
	if !ok {
		return nil, nil
	}
	rule, ok := entry.Config.startGate(c.Work.Kind, c.Binding)
	if !ok {
		return nil, nil
	}
	now := t.o.Now()
	pointer := "start/" + string(c.Work.ID)
	gate := startGateName(c.Work.Kind)
	var rec Ask
	pass := false
	err := t.o.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		rec, pass = Ask{}, false
		if id, err := tx.Get(Namespace, pointer); err == nil {
			// Asked before: hold the Work on that request until it is
			// settled. One already approved and settled lets it start.
			prior, err := getAsk(tx, string(id))
			if err == nil {
				rec = prior
				pass = !prior.SettledAt.IsZero() && prior.State == StateAnswered && prior.Answer != nil && prior.Answer.Answer == OptionApprove
				return nil
			}
			if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		seq, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		rec = Ask{ID: startAskID(c.Work.ID, now), Scope: c.Work.ScopeID, Work: c.Work.ID, Provider: entry.Name, Gate: gate, Start: true,
			Question: "Allow " + gate + " to start?", Options: []string{OptionApprove, OptionRefuse}, Context: startContext(c),
			Deadline: now.Add(rule.timeout), Responders: rule.Responders, Surfaces: entry.Config.Surfaces, AskedAt: now, Sequence: seq, State: StateOpen}
		if e := rule.Escalate; e != nil {
			rec.EscalateAt, rec.EscalateTo, rec.Deadline = rec.Deadline, e.Responders, rec.Deadline.Add(e.timeout)
		}
		if err = putAsk(tx, rec); err != nil {
			return err
		}
		if err = tx.Put(Namespace, "work/"+string(c.Work.ID), []byte(rec.ID)); err != nil {
			return err
		}
		return tx.Put(Namespace, pointer, []byte(rec.ID))
	})
	if err != nil || pass {
		return nil, err
	}
	return &extension.StartHold{Reason: "Waiting for a person's approval before " + gate + " starts (answer by " + rec.Deadline.UTC().Format(time.RFC3339) + ")"}, nil
}

// startGateName names what a start approval is for: "the github.issue Work".
func startGateName(kind string) string {
	if kind == "" {
		return "this Work"
	}
	return "the " + kind + " Work"
}

func startAskID(work domain.ID, now time.Time) string {
	h := sha256.Sum256([]byte("start\x00" + string(work) + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h[:16])
}

// startContext shows the responder what would start: the Work, what it is
// in its source's words, its binding and what triggered it.
func startContext(c extension.StartCall) string {
	lines := []string{"Work: " + string(c.Work.ID)}
	if d := c.Work.Display; d != nil {
		if d.Title != "" {
			lines = append(lines, "Title: "+strings.TrimSpace(d.Ref+" "+d.Title))
		}
		if d.Requester != "" {
			lines = append(lines, "Asked by: "+d.Requester)
		}
		for _, l := range d.Links {
			lines = append(lines, "Link: "+l.URL)
		}
	}
	lines = append(lines, "Binding: "+c.Binding)
	if c.Event.Type != "" {
		lines = append(lines, "Triggered by: "+c.Event.Type)
	}
	text := strings.Join(lines, "\n")
	if len(text) > maxContext {
		text = strings.ToValidUTF8(text[:maxContext], "")
	}
	return text
}

// SettleFunc settles the hold the extension's start gate placed on work
// (inproc.Host.SettleHold).
type SettleFunc func(ctx context.Context, work domain.ID, admit bool, reason, key string) (domain.ID, error)

// SettleStarts settles every start approval request that has been decided
// and not yet settled: an approval lets its Work start once, and a refusal
// or no answer by the deadline cancels it. A request whose Work has ended
// meanwhile is marked settled without asking the host. It is idempotent:
// each settlement's key is its request, and a failed one is retried on the
// next pass. It returns how many it settled.
func SettleStarts(ctx context.Context, store storage.Store, now time.Time, settle SettleFunc) (int, error) {
	var pending []Ask
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		pending = nil
		rows, err := tx.List(Namespace, "ask/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var a Ask
			if json.Unmarshal(row.Value, &a) != nil || !a.Start || !a.SettledAt.IsZero() {
				continue
			}
			if a.State == StateAnswered || a.State == StateExpired {
				pending = append(pending, a)
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	settled := 0
	var errs []error
	for _, a := range pending {
		ended := false
		if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			w, err := tx.Work(a.Work)
			ended = errors.Is(err, storage.ErrNotFound) || err == nil && w.State.Terminal()
			if err != nil && !ended {
				return err
			}
			return nil
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		if !ended {
			admit, reason := startDecision(a)
			if _, err := settle(ctx, a.Work, admit, reason, a.ID); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			cur, err := getAsk(tx, a.ID)
			if err != nil {
				return err
			}
			cur.SettledAt = now
			return putAsk(tx, cur)
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		settled++
	}
	return settled, errors.Join(errs...)
}

// startDecision is what a decided start request settles to, and why.
func startDecision(a Ask) (admit bool, reason string) {
	if a.State == StateAnswered && a.Answer != nil {
		if a.Answer.Answer == OptionApprove {
			return true, a.Answer.Responder + " approved the start"
		}
		return false, a.Answer.Responder + " refused the start"
	}
	return false, "nobody approved the start by " + a.Deadline.UTC().Format(time.RFC3339)
}
