package storage

import (
	"slices"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// ChangeLag is how far a change cursor (NextChangeCursor) stays behind the
// latest change a reader has read. A Work's UpdatedAt is the time its
// transaction ran, not when it committed, so a change committed just after
// a read can carry a time before the latest one that read saw. Reading
// again from ChangeLag before that time reads it, provided no
// transaction takes longer than ChangeLag to commit.
const ChangeLag = time.Minute

var finishedWorkStates = []domain.WorkState{domain.WorkCompleted, domain.WorkFailed, domain.WorkCancelled}

// LiveOrChanged lists the Work q selects that is live (not Terminal), or
// that changed at or after since, each once, in ID order: what a reader
// that mirrors Work elsewhere (a projection) has to look at on each pass.
// It is two indexed QueryWorks, so its cost follows the live set and the
// Work changed since the cursor, not every Work ever made. A zero since
// selects every Work, once, as a reader's first pass. q's States,
// UpdatedSince, Order, After and Limit are ignored.
func LiveOrChanged(tx Tx, q WorkQuery, since time.Time) ([]domain.Work, error) {
	q.Order, q.After, q.Limit = "", nil, 0
	live := q
	live.States, live.UpdatedSince = LiveWorkStates, time.Time{}
	works, err := tx.QueryWorks(live)
	if err != nil {
		return nil, err
	}
	// Finished Work by when it changed, so a store's (state, updated)
	// index reads only the changes since the cursor.
	changed := q
	changed.States, changed.UpdatedSince, changed.Order = finishedWorkStates, since, WorkUpdated
	more, err := tx.QueryWorks(changed)
	if err != nil {
		return nil, err
	}
	seen := make(map[domain.ID]bool, len(works))
	for _, w := range works {
		seen[w.ID] = true
	}
	for _, w := range more {
		if !seen[w.ID] {
			seen[w.ID] = true
			works = append(works, w)
		}
	}
	slices.SortFunc(works, func(a, b domain.Work) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return works, nil
}

// NextChangeCursor is the since of a reader's next LiveOrChanged pass,
// after a pass from since read works and settled all of them except those
// unsettled says it hasn't. It is ChangeLag before the latest change read,
// and no later than the change of the earliest finished Work left
// unsettled, so the next pass reads that Work again; live Work is read
// every pass whatever the cursor. It never moves before since: every
// change committed after the pass that chose since carries a later time.
// A reader that stops a pass part way keeps since instead.
func NextChangeCursor(since time.Time, works []domain.Work, unsettled func(domain.Work) bool) time.Time {
	var latest time.Time
	for _, w := range works {
		if w.UpdatedAt.After(latest) {
			latest = w.UpdatedAt
		}
	}
	if latest.IsZero() {
		return since
	}
	next := latest.Add(-ChangeLag)
	for _, w := range works {
		if w.State.Terminal() && unsettled(w) && w.UpdatedAt.Before(next) {
			next = w.UpdatedAt
		}
	}
	if next.Before(since) {
		return since
	}
	return next
}
