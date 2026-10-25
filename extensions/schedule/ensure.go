package schedule

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// Armer is the runtime's trusted timer control (runtime.Timers): arming a
// configured independent root with a calendar recurrence, and revisioned
// cancellation.
type Armer interface {
	ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error
	Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error
}

// Entry is one configured schedule entry in the current generation.
type Entry struct {
	Scope domain.ID
	// Enabled is the scope's effective enabled state. A disabled scope's
	// timers stay durable and untouched; their due Events resume on
	// re-enable (B05).
	Enabled  bool
	Settings Settings
}

// EnsureResult counts one reconciliation pass.
type EnsureResult struct {
	Armed, Rearmed, Cancelled, Kept int
	// Codes are safe per-timer failure codes; the next pass retries.
	Codes []string
}

// Ensure reconciles every schedule timer with the current generation, the
// reload behavior of 05-01 (v1 refresh): a new timed job gets its timer, an
// edited timing rearms (Desired), an unchanged one is left alone, and a timer
// whose job or entry was removed is cancelled while a run already going
// carries on. It is idempotent and restart-safe: timers are durable, so a
// pass after a crash repeats only what did not commit.
func Ensure(ctx context.Context, store storage.Store, now time.Time, armer Armer, entries []Entry) (EnsureResult, error) {
	var res EnsureResult
	current := map[domain.ID]domain.Timer{}
	err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		all, err := tx.ListTimers()
		for _, t := range all {
			if strings.HasPrefix(string(t.ID), "schedule/") && t.Event.Type == TypeSlot {
				current[t.ID] = t
			}
		}
		return err
	})
	if err != nil {
		return res, err
	}
	byNamespace := map[string]Entry{}
	handled := map[domain.ID]bool{}
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Settings.Namespace < sorted[j].Settings.Namespace })
	fail := func(code string) { res.Codes = append(res.Codes, code) }
	for _, e := range sorted {
		byNamespace[e.Settings.Namespace] = e
		if !e.Enabled {
			continue
		}
		for _, j := range e.Settings.Jobs {
			if !j.Recurring() {
				continue
			}
			id := domain.ID(TimerID(e.Settings.Namespace, j.Name))
			handled[id] = true
			var cur *domain.Timer
			if t, ok := current[id]; ok {
				cur = &t
			}
			want, spec, changed := Desired(e.Settings.Namespace, j, now, cur)
			switch {
			case !changed:
				res.Kept++
			case want == nil:
				if err := armer.Cancel(ctx, id, cur.Revision); err != nil {
					fail(code(err))
					continue
				}
				res.Cancelled++
			default:
				var expected domain.Revision
				if cur != nil {
					expected = cur.Revision
				}
				if err := armer.ArmRecurrence(ctx, e.Scope, *want, expected, RecurrenceKind, spec); err != nil {
					fail(code(err))
					continue
				}
				if cur != nil {
					res.Rearmed++
				} else {
					res.Armed++
				}
			}
		}
	}
	ids := make([]domain.ID, 0, len(current))
	for id := range current {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if handled[id] {
			continue
		}
		t := current[id]
		if e, ok := byNamespace[t.Event.Source]; ok && !e.Enabled {
			res.Kept++
			continue
		}
		if err := armer.Cancel(ctx, id, t.Revision); err != nil {
			fail(code(err))
			continue
		}
		res.Cancelled++
	}
	return res, nil
}

func code(err error) string {
	switch {
	case errors.Is(err, storage.ErrConflict):
		return "conflict"
	case errors.Is(err, storage.ErrInvalid):
		return "invalid_timer"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	}
	return "timer_failed"
}
