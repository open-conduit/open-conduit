package runtime

import (
	"context"
	"errors"
	"reflect"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// Work displays outside a route decision (issue #134). Input a Wait takes
// never reaches a route policy, yet a waiting Work's source can still say
// what the Work is; and Work created before sources gave displays has none
// until its source routes again. Both ask the routes for a display alone:
// the input goes where it would have gone, and a route that errs or gives
// no valid display leaves the Work as it is.

const displayBackfillNamespace = "core.routing.display_backfill"

// displayBackfillTail bounds how much of a Work's history the backfill
// reads: its latest keyed Events, newest first, until one says what it is.
const displayBackfillTail = 50

// displayOnly is the display the routes give s.event, or nil. Nothing else
// about the decision applies, and an error is never surfaced.
func (r *Router) displayOnly(ctx context.Context, s routeSnapshot) *domain.WorkDisplay {
	if s.work == nil || s.work.State.Terminal() {
		return nil
	}
	p, err := r.evaluate(ctx, s, nil)
	if err != nil || p.selected == nil {
		return nil
	}
	return p.decision.Display
}

// putDisplayInTx stores d on the Work seen as was, unless something changed
// its display since (a later route will say again) or it has ended.
func putDisplayInTx(tx storage.Tx, was domain.Work, d *domain.WorkDisplay) error {
	if d == nil {
		return nil
	}
	w, err := tx.Work(was.ID)
	if err != nil {
		return err
	}
	if w.State.Terminal() || !reflect.DeepEqual(w.Display, was.Display) || reflect.DeepEqual(w.Display, d) {
		return nil
	}
	w.Display = d
	return tx.PutWork(w, w.Revision)
}

// BackfillDisplays gives open Work without a display one from its accepted
// history, once per store: for each such Work it asks the routes about its
// latest keyed Events, newest first, until one says what the Work is, then
// replays the Events after that one so each can add to it (a PR's link).
// No service is called. It reports how many Work it gave a display.
func (r *Router) BackfillDisplays(ctx context.Context) (int, error) {
	const marker = "v1"
	var done bool
	needs := map[domain.ID]domain.Work{}
	keys := map[domain.CorrelationKey]domain.ID{}
	err := r.transact(ctx, nil, func(tx storage.Tx) error {
		var marked struct{}
		e := getJSON(tx, displayBackfillNamespace, marker, &marked)
		if done = e == nil; done {
			return nil
		}
		if !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		works, e := tx.ListWorks()
		for _, w := range works {
			if w.Display != nil || w.State.Terminal() {
				continue
			}
			needs[w.ID] = w
			for _, k := range w.CorrelationKeys {
				keys[k] = w.ID
			}
		}
		return e
	})
	if err != nil || done {
		return 0, err
	}
	tails := map[domain.ID][]domain.ID{}
	for after := uint64(0); len(needs) > 0; {
		var page []domain.AcceptedEvent
		if err = r.transact(ctx, nil, func(tx storage.Tx) error { var e error; page, e = tx.Events(after, 500); return e }); err != nil {
			return 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			after = a.Sequence
			id := a.RoutedWorkID
			if a.Event.CorrelationKey != nil && keys[*a.Event.CorrelationKey] != "" {
				id = keys[*a.Event.CorrelationKey]
			}
			if _, ok := needs[id]; !ok {
				continue
			}
			tail := append(tails[id], a.Event.ID)
			if len(tail) > displayBackfillTail {
				tail = tail[1:]
			}
			tails[id] = tail
		}
	}
	given := 0
	for id, tail := range tails {
		if err = ctx.Err(); err != nil {
			return given, err
		}
		w := needs[id]
		if d := r.historyDisplay(ctx, w, tail); d != nil {
			ok := false
			if err = r.transact(ctx, nil, func(tx storage.Tx) error {
				cur, e := tx.Work(id)
				if e != nil {
					return e
				}
				if cur.Display != nil || cur.State.Terminal() {
					return nil
				}
				cur.Display, ok = d, true
				return tx.PutWork(cur, cur.Revision)
			}); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return given, err
			}
			if ok && err == nil {
				given++
			}
		}
	}
	return given, r.transact(ctx, nil, func(tx storage.Tx) error {
		return putJSON(tx, displayBackfillNamespace, marker, struct{}{})
	})
}

// historyDisplay replays a Work's Events (oldest first) for a display:
// from the newest one that says what the Work is on its own, through the
// rest. nil when none does.
func (r *Router) historyDisplay(ctx context.Context, w domain.Work, events []domain.ID) *domain.WorkDisplay {
	ask := func(id domain.ID, d *domain.WorkDisplay) *domain.WorkDisplay {
		s := routeSnapshot{}
		err := r.transact(ctx, nil, func(tx storage.Tx) error {
			var e error
			if s.event, e = tx.Event(id); e != nil {
				return e
			}
			return routeContext(tx, &s)
		})
		if err != nil {
			return nil
		}
		work := w
		work.Display = d
		s.work = &work
		return r.displayOnly(ctx, s)
	}
	for i := len(events) - 1; i >= 0; i-- {
		d := ask(events[i], nil)
		if d == nil {
			continue
		}
		for _, id := range events[i+1:] {
			if next := ask(id, d); next != nil {
				d = next
			}
		}
		return d
	}
	return nil
}
